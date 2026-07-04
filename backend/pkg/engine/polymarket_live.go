package engine

// Live trading integration for Polymarket CLOB.
// Handles EIP-712 order signing, L1/L2 auth, and order submission.
// Only activated when POLY_PRIVATE_KEY and POLY_LIVE=true are set in env.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/hmac"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// Polymarket CTF Exchange V2 (Polygon mainnet, since the April 2026 CLOB V2 migration).
// Binary btc-updown markets are standard (negRisk=false); neg-risk multi-outcome
// markets settle through a different exchange contract.
const (
	clobExchangeAddrV2        = "0xE111180000d2663C0091e4f400237545B87B996B"
	clobExchangeAddrV2NegRisk = "0xe2222d279d744050d28e00520010520000310F59"
	polyChainID               = 137
)

// Order side constants
const (
	sideYES = 0 // BUY
	sideSELL = 1
)

type polymarketCredentials struct {
	signerAddress string // EOA address derived from private key — signs orders
	funderAddress string // proxy wallet address — holds USDC, set as maker in orders
	apiKey        string
	secret        string
	passphrase    string
	signatureType int // 0=EOA/MetaMask, 1=Magic.link
}

// loadPrivateKey parses a hex private key and returns the key + derived wallet address.
func loadPrivateKey(hexKey string) (*ecdsa.PrivateKey, string, error) {
	hexKey = strings.TrimPrefix(hexKey, "0x")
	key, err := gethcrypto.HexToECDSA(hexKey)
	if err != nil {
		return nil, "", fmt.Errorf("invalid POLY_PRIVATE_KEY: %w", err)
	}
	address := gethcrypto.PubkeyToAddress(key.PublicKey).Hex()
	return key, address, nil
}

// deriveOrFetchCredentials obtains Polymarket L2 API credentials.
// It calls GET /auth/api-key first; if none exist it creates them via POST.
//
// L1 auth uses an EIP-712 typed signature of the ClobAuth struct (matching the
// official py-clob-client), NOT a personal_sign of the timestamp.
//
// Address binding: for EOA/proxy accounts the API key is bound to the EOA.
// For POLY_1271 deposit wallets the key must be bound to the deposit wallet
// itself — the CLOB requires order.signer == API-key address, and 1271 orders
// carry the contract as signer. The ClobAuth signature still comes from the
// EOA; the server validates it via EIP-1271 against the deposit wallet.
func deriveOrFetchCredentials(key *ecdsa.PrivateKey, signerAddr, funderAddr string, sigType int) (*polymarketCredentials, error) {
	const clobBase = "https://clob.polymarket.com"
	ts := fmt.Sprintf("%d", time.Now().Unix())

	// API keys are strictly EOA-bound: the CLOB verifies the ClobAuth signature
	// by ECDSA recovery against POLY_ADDRESS (a wallet-address binding 401s).
	authAddr := signerAddr

	sig, err := signClobAuth(key, authAddr, ts, 0)
	if err != nil {
		return nil, fmt.Errorf("L1 sign failed: %w", err)
	}

	headers := map[string]string{
		"POLY_ADDRESS":   authAddr,
		"POLY_SIGNATURE": sig,
		"POLY_TIMESTAMP": ts,
		"POLY_NONCE":     "0",
		"Content-Type":   "application/json",
	}

	// GET derives the existing key; POST creates one on first use. Either can
	// fail transiently (flaky DNS/network at startup), and a failed GET followed
	// by a POST against an existing key 400s — so retry the whole sequence.
	var creds *polymarketCredentials
	var getErr, postErr error
	for attempt := 1; attempt <= 3; attempt++ {
		creds, getErr = fetchAPIKey(clobBase+"/auth/derive-api-key", headers)
		if getErr == nil {
			creds.signerAddress = authAddr
			creds.signatureType = sigType
			return creds, nil
		}
		creds, postErr = createAPIKey(clobBase+"/auth/api-key", headers, nil)
		if postErr == nil {
			creds.signerAddress = authAddr
			creds.signatureType = sigType
			return creds, nil
		}
		time.Sleep(time.Duration(attempt) * 2 * time.Second)
	}
	return nil, fmt.Errorf("failed to obtain Polymarket API key after 3 attempts (derive: %v; create: %w)", getErr, postErr)
}

// signClobAuth builds the EIP-712 ClobAuth attestation signature Polymarket's
// /auth endpoints require:
//
//	domain: { name: "ClobAuthDomain", version: "1", chainId: 137 }  (no verifyingContract)
//	ClobAuth(address address,string timestamp,uint256 nonce,string message)
//	message: "This message attests that I control the given wallet"
func signClobAuth(key *ecdsa.PrivateKey, address, timestamp string, nonce int64) (string, error) {
	domainTypeHash := gethcrypto.Keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId)",
	))
	domainSep := gethcrypto.Keccak256(concat(
		domainTypeHash,
		gethcrypto.Keccak256([]byte("ClobAuthDomain")),
		gethcrypto.Keccak256([]byte("1")),
		pad32(big.NewInt(polyChainID)),
	))

	authTypeHash := gethcrypto.Keccak256([]byte(
		"ClobAuth(address address,string timestamp,uint256 nonce,string message)",
	))
	structHash := gethcrypto.Keccak256(concat(
		authTypeHash,
		hexToBytes32(address),
		gethcrypto.Keccak256([]byte(timestamp)),
		pad32(big.NewInt(nonce)),
		gethcrypto.Keccak256([]byte("This message attests that I control the given wallet")),
	))

	digest := gethcrypto.Keccak256(concat([]byte("\x19\x01"), domainSep, structHash))
	sig, err := gethcrypto.Sign(digest, key)
	if err != nil {
		return "", err
	}
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig), nil
}

func fetchAPIKey(url string, headers map[string]string) (*polymarketCredentials, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("build GET request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	var result struct {
		ApiKey     string `json:"apiKey"`
		Secret     string `json:"secret"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if result.ApiKey == "" {
		return nil, fmt.Errorf("empty apiKey in response")
	}
	return &polymarketCredentials{
		apiKey:     result.ApiKey,
		secret:     result.Secret,
		passphrase: result.Passphrase,
	}, nil
}

func createAPIKey(url string, headers map[string]string, body []byte) (*polymarketCredentials, error) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build POST request: %w", err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return nil, fmt.Errorf("status %d: %s", resp.StatusCode, string(bodyBytes))
	}
	var result struct {
		ApiKey     string `json:"apiKey"`
		Secret     string `json:"secret"`
		Passphrase string `json:"passphrase"`
	}
	if err := json.Unmarshal(bodyBytes, &result); err != nil {
		return nil, err
	}
	return &polymarketCredentials{
		apiKey:     result.ApiKey,
		secret:     result.Secret,
		passphrase: result.Passphrase,
	}, nil
}

// personalSign signs a message using Ethereum's personal_sign format:
// keccak256("\x19Ethereum Signed Message:\n" + len(msg) + msg)
func personalSign(msg []byte, key *ecdsa.PrivateKey) (string, error) {
	prefix := fmt.Sprintf("\x19Ethereum Signed Message:\n%d", len(msg))
	data := append([]byte(prefix), msg...)
	hash := gethcrypto.Keccak256(data)
	sig, err := gethcrypto.Sign(hash, key)
	if err != nil {
		return "", err
	}
	// Adjust recovery bit (Ethereum uses 27/28, crypto.Sign returns 0/1)
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig), nil
}

// buildL2Headers constructs the HMAC-SHA256 authentication headers for L2 auth.
// POLY_ADDRESS is the signer (EOA) address — the CLOB ties API keys to the signer, not the funder.
// Matching py-clob-client: the secret is base64-urlsafe-decoded to get the HMAC key,
// and the digest is base64-urlsafe-encoded (NOT hex). path is the bare request path
// without query parameters.
func buildL2Headers(creds *polymarketCredentials, method, path, body string) map[string]string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	msg := ts + strings.ToUpper(method) + path + body

	secretKey, err := base64.URLEncoding.DecodeString(creds.secret)
	if err != nil {
		// Some secrets arrive unpadded; retry with raw (no-padding) alphabet
		secretKey, err = base64.RawURLEncoding.DecodeString(creds.secret)
		if err != nil {
			secretKey = []byte(creds.secret) // last resort: raw string
		}
	}

	mac := hmac.New(sha256.New, secretKey)
	mac.Write([]byte(msg))
	sig := base64.URLEncoding.EncodeToString(mac.Sum(nil))

	return map[string]string{
		"POLY_ADDRESS":    creds.signerAddress,
		"POLY_API_KEY":    creds.apiKey,
		"POLY_PASSPHRASE": creds.passphrase,
		"POLY_TIMESTAMP":  ts,
		"POLY_NONCE":      "0",
		"POLY_SIGNATURE":  sig,
		"Content-Type":    "application/json",
	}
}

// ---------------------------------------------------------------------------
// EIP-712 Order Signing
// ---------------------------------------------------------------------------

// orderTypeHash is keccak256 of the CLOB V2 Order type string.
// V2 dropped taker/expiration/nonce/feeRateBps from the signed struct and
// added timestamp (ms, replaces nonce for uniqueness), metadata and builder.
var orderTypeHash = gethcrypto.Keccak256([]byte(
	"Order(uint256 salt,address maker,address signer,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint8 side,uint8 signatureType,uint256 timestamp,bytes32 metadata,bytes32 builder)",
))

// V2 domain separators (version "2"); the ClobAuthDomain used for L1 auth stays at v1.
var (
	domainSeparatorV2        = computeDomainSeparator(clobExchangeAddrV2)
	domainSeparatorV2NegRisk = computeDomainSeparator(clobExchangeAddrV2NegRisk)
)

func computeDomainSeparator(exchangeAddr string) []byte {
	domainTypeHash := gethcrypto.Keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))
	nameHash := gethcrypto.Keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := gethcrypto.Keccak256([]byte("2"))
	chainID := pad32(big.NewInt(polyChainID))
	exchange := hexToBytes32(exchangeAddr)
	packed := concat(domainTypeHash, nameHash, versionHash, chainID, exchange)
	return gethcrypto.Keccak256(packed)
}

type clobOrder struct {
	Salt          *big.Int
	Maker         string
	Signer        string
	TokenID       *big.Int
	MakerAmount   *big.Int // USDC in 6-decimal units
	TakerAmount   *big.Int // shares in 6-decimal units
	Side          uint8    // 0=BUY, 1=SELL
	SignatureType uint8    // 0=EOA, 1=Magic.link
	Timestamp     *big.Int // order creation time in milliseconds
	Metadata      [32]byte
	Builder       [32]byte // zero unless attaching a builder code
}

// signEIP712Order computes the V2 EIP-712 hash of an order and signs it.
// negRisk selects the exchange contract the order settles through.
func signEIP712Order(order clobOrder, key *ecdsa.PrivateKey, negRisk bool) (string, error) {
	structHash := gethcrypto.Keccak256(concat(
		orderTypeHash,
		pad32(order.Salt),
		hexToBytes32(order.Maker),
		hexToBytes32(order.Signer),
		pad32(order.TokenID),
		pad32(order.MakerAmount),
		pad32(order.TakerAmount),
		padUint8(order.Side),
		padUint8(order.SignatureType),
		pad32(order.Timestamp),
		order.Metadata[:], // bytes32 encodes as its raw 32 bytes
		order.Builder[:],
	))

	domainSep := domainSeparatorV2
	if negRisk {
		domainSep = domainSeparatorV2NegRisk
	}

	digest := gethcrypto.Keccak256(concat(
		[]byte("\x19\x01"),
		domainSep,
		structHash,
	))

	sig, err := gethcrypto.Sign(digest, key)
	if err != nil {
		return "", err
	}
	// Ethereum convention: v = 27 + recovery
	sig[64] += 27
	return "0x" + hex.EncodeToString(sig), nil
}

// ---------------------------------------------------------------------------
// Order Submission
// ---------------------------------------------------------------------------

type clobOrderRequest struct {
	Order     clobOrderJSON `json:"order"`
	Owner     string        `json:"owner"`
	OrderType string        `json:"orderType"` // "FOK", "GTC", "GTD"
	DeferExec bool          `json:"deferExec"`
	PostOnly  bool          `json:"postOnly"`
}

// clobOrderJSON is the V2 wire format (mirrors py-clob-client-v2
// order_to_json_v2: salt is a JSON number, everything else strings/ints).
// expiration stays in the body for GTD handling but is NOT part of the
// signed struct.
type clobOrderJSON struct {
	Salt          int64  `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Side          string `json:"side"`
	SignatureType int    `json:"signatureType"`
	Timestamp     string `json:"timestamp"` // milliseconds, matches signed struct
	Metadata      string `json:"metadata"`
	Builder       string `json:"builder"`
	Signature     string `json:"signature"`
}

// submitLiveBuyOrder sends a real BUY limit order to the Polymarket CLOB.
// usdcBudget is the maximum USDC spend; the order is sized to whole shares at
// the tick price so the signed limit price equals the intended entry price.
// Returns the CLOB order ID, filled share count and actual USDC cost.
func (p *PolymarketEngine) submitLiveBuyOrder(tokenID string, price, usdcBudget float64) (string, float64, float64, error) {
	tickPrice := math.Round(price*100) / 100
	if tickPrice <= 0 || tickPrice >= 1 {
		return "", 0, 0, fmt.Errorf("price %.4f out of valid range (0,1) after rounding", tickPrice)
	}
	shares := math.Floor(usdcBudget / tickPrice)
	if shares < 1 {
		return "", 0, 0, fmt.Errorf("order too small: %.2f USDC at %.2f yields %.2f shares", usdcBudget, tickPrice, usdcBudget/tickPrice)
	}
	orderID, err := p.submitLiveOrder(tokenID, tickPrice, shares, 0, "BUY")
	if err != nil {
		return "", 0, 0, err
	}
	return orderID, shares, shares * tickPrice, nil
}

// submitLiveSellOrder sends a real SELL limit order to the Polymarket CLOB.
// shares must not exceed the shares actually held (whole shares from the buy fill).
func (p *PolymarketEngine) submitLiveSellOrder(tokenID string, price, shares float64) (string, error) {
	tickPrice := math.Round(price*100) / 100
	if tickPrice <= 0 || tickPrice >= 1 {
		return "", fmt.Errorf("price %.4f out of valid range (0,1) after rounding", tickPrice)
	}
	wholeShares := math.Floor(shares)
	if wholeShares < 1 {
		return "", fmt.Errorf("sell too small: %.2f shares", shares)
	}
	return p.submitLiveOrder(tokenID, tickPrice, wholeShares, 1, "SELL")
}

// submitLiveOrder signs and posts a FOK order for a whole number of shares at
// tickPrice (already rounded to the $0.01 tick by the callers above).
func (p *PolymarketEngine) submitLiveOrder(tokenID string, tickPrice, shares float64, side uint8, sideStr string) (string, error) {
	if p.creds == nil || p.privateKey == nil {
		return "", fmt.Errorf("live trading not initialized")
	}

	// Amounts in 6-decimal units. USDC leg is shares*price so the implied
	// limit price (makerAmount/takerAmount) is exactly the intended tick price.
	// BUY:  maker gives USDC → receives shares.  makerAmt=USDC, takerAmt=shares.
	// SELL: maker gives shares → receives USDC.  makerAmt=shares, takerAmt=USDC.
	usdcRaw := new(big.Int).SetInt64(int64(math.Round(shares * tickPrice * 1e6)))
	sharesRaw := new(big.Int).SetInt64(int64(math.Round(shares * 1e6)))
	var makerAmtRaw, takerAmtRaw *big.Int
	if side == 1 { // SELL
		makerAmtRaw = sharesRaw
		takerAmtRaw = usdcRaw
	} else { // BUY
		makerAmtRaw = usdcRaw
		takerAmtRaw = sharesRaw
	}

	tokenBig, ok := new(big.Int).SetString(tokenID, 10)
	if !ok {
		return "", fmt.Errorf("invalid tokenID %q", tokenID)
	}

	// Use crypto/rand for salt — math/rand is deterministic and unsuitable for order security.
	// Capped below 2^53 so it survives the JSON number round-trip (py-clob-client wire format).
	saltMax := new(big.Int).Lsh(big.NewInt(1), 53)
	salt, err := crand.Int(crand.Reader, saltMax)
	if err != nil {
		return "", fmt.Errorf("generate order salt: %w", err)
	}

	// Maker is always the wallet holding USDC (funder when set, else the EOA).
	// Signer depends on signature type (mirrors py-clob-client-v2 _v2_order_signer):
	//   POLY_1271 (3): signer = funder — the deposit wallet contract validates the
	//                  EOA's signature via EIP-1271, and the CLOB requires
	//                  order.signer == the API-key-bound address (the wallet).
	//   otherwise:     signer = EOA address derived from the private key.
	makerAddr := p.walletAddress
	if p.funderAddress != "" {
		makerAddr = p.funderAddress
	}
	orderSigner := p.walletAddress
	if p.creds.signatureType == 3 && p.funderAddress != "" {
		orderSigner = p.funderAddress
	}

	sigType := uint8(p.creds.signatureType)
	nowMs := big.NewInt(time.Now().UnixMilli())
	var zero32 [32]byte
	order := clobOrder{
		Salt:          salt,
		Maker:         makerAddr,
		Signer:        orderSigner,
		TokenID:       tokenBig,
		MakerAmount:   makerAmtRaw,
		TakerAmount:   takerAmtRaw,
		Side:          side,
		SignatureType: sigType,
		Timestamp:     nowMs,
		Metadata:      zero32,
		Builder:       zero32,
	}

	p.mu.RLock()
	negRisk := p.negRiskMarket
	p.mu.RUnlock()

	sig, err := signEIP712Order(order, p.privateKey, negRisk)
	if err != nil {
		return "", fmt.Errorf("EIP-712 sign failed: %w", err)
	}

	zeroBytes32Hex := "0x0000000000000000000000000000000000000000000000000000000000000000"
	payload := clobOrderRequest{
		Order: clobOrderJSON{
			Salt:          salt.Int64(), // capped < 2^53 at generation
			Maker:         makerAddr,
			Signer:        orderSigner,
			TokenID:       tokenID,
			MakerAmount:   makerAmtRaw.String(),
			TakerAmount:   takerAmtRaw.String(),
			Expiration:    "0",
			Side:          sideStr,
			SignatureType: p.creds.signatureType,
			Timestamp:     nowMs.String(),
			Metadata:      zeroBytes32Hex,
			Builder:       zeroBytes32Hex,
			Signature:     sig,
		},
		Owner:     p.creds.apiKey, // owner is the API key, not an address
		OrderType: "FOK",
		DeferExec: false,
		PostOnly:  false,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal order: %w", err)
	}

	bodyStr := string(bodyBytes)
	headers := buildL2Headers(p.creds, "POST", "/order", bodyStr)

	req, err := http.NewRequest("POST", p.clobURL+"/order", bytes.NewReader(bodyBytes))
	if err != nil {
		return "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return "", fmt.Errorf("CLOB POST /order failed: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		// On a fill failure, snapshot the live book so we can tell a pricing
		// problem (our limit sits inside the spread) from a depth problem (the
		// book genuinely can't fill our size) — the two need opposite fixes.
		if strings.Contains(string(respBody), "fully filled") {
			p.logBookContext(tokenID, side, shares, tickPrice)
		}
		return "", fmt.Errorf("CLOB returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		OrderID    string `json:"orderID"`
		Status     string `json:"status"`
		SuccessMsg string `json:"successMsg"`
		ErrorMsg   string `json:"errorMsg"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decode CLOB response: %w (body: %s)", err, string(respBody))
	}

	// Log the raw fill response once per order so we can learn the exact schema
	// (making/taking amounts) and reconcile actual fill price vs our limit.
	p.store.Log("INFO", fmt.Sprintf("[CLOB Fill] raw response: %s", string(respBody)))

	if result.ErrorMsg != "" {
		return "", fmt.Errorf("CLOB order rejected: %s", result.ErrorMsg)
	}

	// FOK may return empty orderID if it cancelled (no matching asks)
	if result.OrderID == "" && result.Status != "matched" {
		return "", fmt.Errorf("FOK order not filled (no matching asks at %.2f)", tickPrice)
	}

	return result.OrderID, nil
}

// logBookContext fetches the live order book for a token and logs whether our
// order could theoretically fill: the best ask, the price that would sweep our
// full share count, and the total marketable depth. Diagnostic only.
func (p *PolymarketEngine) logBookContext(tokenID string, side uint8, shares, limitPrice float64) {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(p.clobURL + "/book?token_id=" + tokenID)
	if err != nil {
		p.store.Log("WARN", fmt.Sprintf("[CLOB Book] fetch failed for fill diagnosis: %v", err))
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var book struct {
		Bids []struct{ Price, Size string } `json:"bids"`
		Asks []struct{ Price, Size string } `json:"asks"`
	}
	if err := json.Unmarshal(body, &book); err != nil {
		p.store.Log("WARN", fmt.Sprintf("[CLOB Book] parse failed: %v", err))
		return
	}

	pf := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }

	// A BUY sweeps asks from the lowest price up; a SELL sweeps bids from the
	// highest price down. Accumulate size until we cover our share count and
	// record the worst price consumed (the effective marketable limit).
	levels := book.Asks
	if side == 1 {
		levels = book.Bids
	}
	// Sort so index 0 is the most aggressive level we'd hit first.
	sort.Slice(levels, func(i, j int) bool {
		if side == 1 {
			return pf(levels[i].Price) > pf(levels[j].Price) // bids: high→low
		}
		return pf(levels[i].Price) < pf(levels[j].Price) // asks: low→high
	})

	best := 0.0
	if len(levels) > 0 {
		best = pf(levels[0].Price)
	}
	var cum, sweep float64
	filled := false
	for _, lvl := range levels {
		cum += pf(lvl.Size)
		sweep = pf(lvl.Price)
		if cum >= shares {
			filled = true
			break
		}
	}

	sideStr := "BUY(asks)"
	if side == 1 {
		sideStr = "SELL(bids)"
	}
	if filled {
		p.store.Log("INFO", fmt.Sprintf("[CLOB Book] %s need %.0f sh @ limit $%.2f | best $%.3f | full-size sweep price $%.3f (%s) | depth ok",
			sideStr, shares, limitPrice, best, sweep,
			map[bool]string{true: "marketable — limit too passive", false: "within limit"}[sweep > limitPrice]))
	} else {
		p.store.Log("INFO", fmt.Sprintf("[CLOB Book] %s need %.0f sh @ limit $%.2f | best $%.3f | only %.0f sh available total — book too thin",
			sideStr, shares, limitPrice, best, cum))
	}
}

type bookLevel struct {
	price float64
	size  float64
}

// fetchBookLevels returns one side of the live CLOB book, sorted most-aggressive
// first: asks low→high (side 0, what a BUY consumes), bids high→low (side 1, what
// a SELL consumes).
func (p *PolymarketEngine) fetchBookLevels(tokenID string, side int) ([]bookLevel, error) {
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(p.clobURL + "/book?token_id=" + tokenID)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var book struct {
		Bids []struct{ Price, Size string } `json:"bids"`
		Asks []struct{ Price, Size string } `json:"asks"`
	}
	if err := json.Unmarshal(body, &book); err != nil {
		return nil, err
	}
	raw := book.Asks
	if side == 1 {
		raw = book.Bids
	}
	pf := func(s string) float64 { f, _ := strconv.ParseFloat(s, 64); return f }
	levels := make([]bookLevel, 0, len(raw))
	for _, l := range raw {
		levels = append(levels, bookLevel{pf(l.Price), pf(l.Size)})
	}
	sort.Slice(levels, func(i, j int) bool {
		if side == 1 {
			return levels[i].price > levels[j].price
		}
		return levels[i].price < levels[j].price
	})
	return levels, nil
}

// GetMarketablePrice reads the live CLOB book and returns the limit price at
// which `shares` of the YES/NO token can be fully filled right now: the worst
// price swept, ceil'd to the $0.01 tick for a BUY (side 0) so the order crosses,
// floor'd for a SELL (side 1). fillable is false when the book lacks the depth.
// This is the real executable price — used to re-check edge before committing,
// replacing the lagged last-trade feed the strategy computes EV from.
func (p *PolymarketEngine) GetMarketablePrice(isYes bool, side int, shares float64) (price float64, fillable bool, err error) {
	p.mu.RLock()
	tokenID := p.yesTokenID
	if !isYes {
		tokenID = p.noTokenID
	}
	p.mu.RUnlock()
	if tokenID == "" {
		return 0, false, fmt.Errorf("token id unavailable")
	}

	levels, err := p.fetchBookLevels(tokenID, side)
	if err != nil {
		return 0, false, err
	}
	var cum, sweep float64
	for _, lvl := range levels {
		cum += lvl.size
		sweep = lvl.price
		if cum >= shares {
			if side == 0 {
				return math.Ceil(sweep*100) / 100, true, nil
			}
			return math.Floor(sweep*100) / 100, true, nil
		}
	}
	return 0, false, nil // book too thin for this size
}

// getCLOBBalance fetches the real USDC collateral balance from Polymarket CLOB.
// Endpoint: GET /balance-allowance?asset_type=COLLATERAL&signature_type=N.
// The HMAC signs the bare path (no query string), matching py-clob-client.
// Response balance is in raw 6-decimal units (e.g. "15000000" = $15.00).
func (p *PolymarketEngine) getCLOBBalance() (float64, error) {
	if p.creds == nil {
		return 0, fmt.Errorf("not initialized")
	}

	const path = "/balance-allowance"
	headers := buildL2Headers(p.creds, "GET", path, "")

	url := fmt.Sprintf("%s%s?asset_type=COLLATERAL&signature_type=%d",
		p.clobURL, path, p.creds.signatureType)
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return 0, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("balance-allowance HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var result struct {
		Balance string `json:"balance"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("decode balance response: %w (body: %s)", err, string(respBody))
	}

	var raw float64
	fmt.Sscanf(result.Balance, "%f", &raw)
	return raw / 1e6, nil
}

// resolveTokenForPosition returns the CLOB token ID to sell given a position.
// YES positions → yesTokenID, NO positions → noTokenID.
func (p *PolymarketEngine) resolveTokenForClose(units float64) string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if units > 0 {
		return p.yesTokenID
	}
	return p.noTokenID
}

// ---------------------------------------------------------------------------
// ABI / encoding helpers
// ---------------------------------------------------------------------------

// pad32 left-pads a big.Int to 32 bytes (ABI/EIP-712 uint256 encoding).
// For values > 32 bytes (> 2^256) we take the least-significant 32 bytes — valid
// EIP-712 fields are always ≤ 256 bits, so this path is a safety net only.
func pad32(n *big.Int) []byte {
	b := n.Bytes()
	if len(b) > 32 {
		return b[len(b)-32:]
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// padUint8 pads a uint8 to 32 bytes (ABI encoding).
func padUint8(v uint8) []byte {
	out := make([]byte, 32)
	out[31] = v
	return out
}

// hexToBytes32 converts an Ethereum address hex string to a 32-byte right-padded slice.
func hexToBytes32(addrHex string) []byte {
	addrHex = strings.TrimPrefix(addrHex, "0x")
	b, _ := hex.DecodeString(addrHex)
	out := make([]byte, 32)
	// addresses are 20 bytes, right-aligned in 32 bytes
	copy(out[12:], b)
	return out
}

// concat joins byte slices.
func concat(parts ...[]byte) []byte {
	var total int
	for _, p := range parts {
		total += len(p)
	}
	out := make([]byte, 0, total)
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

// loadLiveCredentials reads POLY_PRIVATE_KEY from env and derives API credentials.
// Returns nil, "", nil, nil when POLY_LIVE != "true" (paper mode).
// signerAddr is the EOA address derived from the private key.
// funderAddr is POLY_FUNDER_ADDRESS (the Magic.link proxy wallet); empty for EOA accounts.
func loadLiveCredentials() (key *ecdsa.PrivateKey, signerAddr, funderAddr string, creds *polymarketCredentials, err error) {
	if os.Getenv("POLY_LIVE") != "true" {
		return nil, "", "", nil, nil
	}
	privHex := os.Getenv("POLY_PRIVATE_KEY")
	if privHex == "" {
		return nil, "", "", nil, fmt.Errorf("POLY_LIVE=true but POLY_PRIVATE_KEY is not set")
	}

	key, signerAddr, err = loadPrivateKey(privHex)
	if err != nil {
		return nil, "", "", nil, err
	}

	funderAddr = os.Getenv("POLY_FUNDER_ADDRESS")

	// V2 signature types: 0=EOA, 1=POLY_PROXY (legacy Magic.link proxy),
	// 2=POLY_GNOSIS_SAFE, 3=POLY_1271 (deposit wallets / smart contract wallets).
	sigType := 0
	if v := os.Getenv("POLY_SIGNATURE_TYPE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= 3 {
			sigType = n
		} else {
			return nil, "", "", nil, fmt.Errorf("invalid POLY_SIGNATURE_TYPE %q (must be 0-3)", v)
		}
	}

	creds, err = deriveOrFetchCredentials(key, signerAddr, funderAddr, sigType)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("derive API credentials: %w", err)
	}
	creds.funderAddress = funderAddr

	return key, signerAddr, funderAddr, creds, nil
}

