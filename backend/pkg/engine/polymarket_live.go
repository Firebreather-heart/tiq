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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	gethcrypto "github.com/ethereum/go-ethereum/crypto"
)

// Polymarket CTF Exchange (Polygon mainnet)
const (
	clobExchangeAddr = "0x4bFb41d5B3570DeFd03C39a9A4D8dE6Bd8B8982E"
	polyChainID      = 137
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
func deriveOrFetchCredentials(key *ecdsa.PrivateKey, address string, sigType int) (*polymarketCredentials, error) {
	const clobBase = "https://clob.polymarket.com"
	ts := fmt.Sprintf("%d", time.Now().Unix())

	// Build L1 auth headers (personal_sign of timestamp)
	sig, err := personalSign([]byte(ts), key)
	if err != nil {
		return nil, fmt.Errorf("L1 sign failed: %w", err)
	}

	headers := map[string]string{
		"POLY_ADDRESS":        address,
		"POLY_SIGNATURE":      sig,
		"POLY_TIMESTAMP":      ts,
		"POLY_NONCE":          "0",
		"Content-Type":        "application/json",
	}

	// Try GET first (returns existing key if already created)
	creds, err := fetchAPIKey(clobBase+"/auth/api-key", headers)
	if err == nil {
		creds.signerAddress = address
		creds.signatureType = sigType
		return creds, nil
	}

	// Create new API key
	body := map[string]interface{}{"geo_block_token": ""}
	bodyBytes, _ := json.Marshal(body)
	creds, err = createAPIKey(clobBase+"/auth/api-key", headers, bodyBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to create Polymarket API key: %w", err)
	}
	creds.signerAddress = address
	creds.signatureType = sigType
	return creds, nil
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
func buildL2Headers(creds *polymarketCredentials, method, path, body string) map[string]string {
	ts := fmt.Sprintf("%d", time.Now().Unix())
	msg := ts + strings.ToUpper(method) + path + body
	mac := hmac.New(sha256.New, []byte(creds.secret))
	mac.Write([]byte(msg))
	sig := hex.EncodeToString(mac.Sum(nil))
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

// orderTypeHash is keccak256 of the Polymarket Order type string.
var orderTypeHash = gethcrypto.Keccak256([]byte(
	"Order(uint256 salt,address maker,address signer,address taker,uint256 tokenId,uint256 makerAmount,uint256 takerAmount,uint256 expiration,uint256 nonce,uint256 feeRateBps,uint8 side,uint8 signatureType)",
))

// domainSeparator for Polymarket CTF Exchange on Polygon mainnet.
var domainSeparator = computeDomainSeparator()

func computeDomainSeparator() []byte {
	domainTypeHash := gethcrypto.Keccak256([]byte(
		"EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)",
	))
	nameHash := gethcrypto.Keccak256([]byte("Polymarket CTF Exchange"))
	versionHash := gethcrypto.Keccak256([]byte("1"))
	chainID := pad32(big.NewInt(polyChainID))
	exchange := hexToBytes32(clobExchangeAddr)
	packed := concat(domainTypeHash, nameHash, versionHash, chainID, exchange)
	return gethcrypto.Keccak256(packed)
}

type clobOrder struct {
	Salt          *big.Int
	Maker         string
	Signer        string
	Taker         string
	TokenID       *big.Int
	MakerAmount   *big.Int // USDC in 6-decimal units
	TakerAmount   *big.Int // shares in 6-decimal units
	Expiration    *big.Int
	Nonce         *big.Int
	FeeRateBps    *big.Int
	Side          uint8    // 0=BUY, 1=SELL
	SignatureType uint8    // 0=EOA, 1=Magic.link
}

// signEIP712Order computes the EIP-712 hash of an order and signs it.
func signEIP712Order(order clobOrder, key *ecdsa.PrivateKey) (string, error) {
	structHash := gethcrypto.Keccak256(concat(
		orderTypeHash,
		pad32(order.Salt),
		hexToBytes32(order.Maker),
		hexToBytes32(order.Signer),
		hexToBytes32(order.Taker),
		pad32(order.TokenID),
		pad32(order.MakerAmount),
		pad32(order.TakerAmount),
		pad32(order.Expiration),
		pad32(order.Nonce),
		pad32(order.FeeRateBps),
		padUint8(order.Side),
		padUint8(order.SignatureType),
	))

	digest := gethcrypto.Keccak256(concat(
		[]byte("\x19\x01"),
		domainSeparator,
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
}

type clobOrderJSON struct {
	Salt          string `json:"salt"`
	Maker         string `json:"maker"`
	Signer        string `json:"signer"`
	Taker         string `json:"taker"`
	TokenID       string `json:"tokenId"`
	MakerAmount   string `json:"makerAmount"`
	TakerAmount   string `json:"takerAmount"`
	Expiration    string `json:"expiration"`
	Nonce         string `json:"nonce"`
	FeeRateBps    string `json:"feeRateBps"`
	Side          string `json:"side"`
	SignatureType int    `json:"signatureType"`
	Signature     string `json:"signature"`
}

// submitLiveBuyOrder sends a real BUY limit order to the Polymarket CLOB.
// price must be on $0.01 tick ($0.35–$0.65). usdcAmount is the USDC spend.
// Returns the CLOB order ID, or "" if the FOK cancelled (no fill).
func (p *PolymarketEngine) submitLiveBuyOrder(tokenID string, price, usdcAmount float64) (string, error) {
	return p.submitLiveOrder(tokenID, price, usdcAmount, 0, "BUY")
}

// submitLiveSellOrder sends a real SELL limit order to the Polymarket CLOB.
func (p *PolymarketEngine) submitLiveSellOrder(tokenID string, price, shares float64) (string, error) {
	usdcAmount := shares * price
	return p.submitLiveOrder(tokenID, price, usdcAmount, 1, "SELL")
}

func (p *PolymarketEngine) submitLiveOrder(tokenID string, price, usdcAmount float64, side uint8, sideStr string) (string, error) {
	if p.creds == nil || p.privateKey == nil {
		return "", fmt.Errorf("live trading not initialized")
	}

	// Enforce $0.01 tick
	tickPrice := math.Round(price*100) / 100
	if tickPrice <= 0 || tickPrice >= 1 {
		return "", fmt.Errorf("price %.4f out of valid range (0,1) after rounding", tickPrice)
	}

	// Calculate shares: floor to 1-share precision
	shares := math.Floor(usdcAmount / tickPrice)
	if shares < 1 {
		return "", fmt.Errorf("order too small: %.2f USDC at %.2f yields %.2f shares", usdcAmount, tickPrice, usdcAmount/tickPrice)
	}

	// Amounts in 6-decimal units.
	// BUY:  maker gives USDC → receives shares.  makerAmt=USDC, takerAmt=shares.
	// SELL: maker gives shares → receives USDC.  makerAmt=shares, takerAmt=USDC.
	usdcRaw := new(big.Int).SetInt64(int64(math.Round(usdcAmount * 1e6)))
	sharesRaw := new(big.Int).SetInt64(int64(shares * 1e6))
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
	saltMax := new(big.Int).Lsh(big.NewInt(1), 128)
	salt, err := crand.Int(crand.Reader, saltMax)
	if err != nil {
		return "", fmt.Errorf("generate order salt: %w", err)
	}

	// For Magic.link accounts: maker = funder (proxy wallet holding USDC),
	// signer = EOA address derived from private key.
	// For EOA accounts: maker = signer = walletAddress.
	makerAddr := p.walletAddress
	if p.funderAddress != "" {
		makerAddr = p.funderAddress
	}

	sigType := uint8(p.creds.signatureType)
	order := clobOrder{
		Salt:          salt,
		Maker:         makerAddr,
		Signer:        p.walletAddress,
		Taker:         "0x0000000000000000000000000000000000000000",
		TokenID:       tokenBig,
		MakerAmount:   makerAmtRaw,
		TakerAmount:   takerAmtRaw,
		Expiration:    big.NewInt(0),
		Nonce:         big.NewInt(0),
		FeeRateBps:    big.NewInt(0),
		Side:          side,
		SignatureType: sigType,
	}

	sig, err := signEIP712Order(order, p.privateKey)
	if err != nil {
		return "", fmt.Errorf("EIP-712 sign failed: %w", err)
	}

	payload := clobOrderRequest{
		Order: clobOrderJSON{
			Salt:          salt.String(),
			Maker:         makerAddr,
			Signer:        p.walletAddress,
			Taker:         "0x0000000000000000000000000000000000000000",
			TokenID:       tokenID,
			MakerAmount:   makerAmtRaw.String(),
			TakerAmount:   takerAmtRaw.String(),
			Expiration:    "0",
			Nonce:         "0",
			FeeRateBps:    "0",
			Side:          sideStr,
			SignatureType: p.creds.signatureType,
			Signature:     sig,
		},
		Owner:     makerAddr,
		OrderType: "FOK",
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

	if result.ErrorMsg != "" {
		return "", fmt.Errorf("CLOB order rejected: %s", result.ErrorMsg)
	}

	// FOK may return empty orderID if it cancelled (no matching asks)
	if result.OrderID == "" && result.Status != "matched" {
		return "", fmt.Errorf("FOK order not filled (no matching asks at %.2f)", tickPrice)
	}

	return result.OrderID, nil
}

// getCLOBBalance fetches the real USDC balance for this wallet from Polymarket CLOB.
func (p *PolymarketEngine) getCLOBBalance() (float64, error) {
	if p.creds == nil {
		return 0, fmt.Errorf("not initialized")
	}

	path := "/balance"
	headers := buildL2Headers(p.creds, "GET", path, "")

	req, _ := http.NewRequest("GET", p.clobURL+path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var result struct {
		Balance string `json:"balance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return 0, err
	}

	var bal float64
	fmt.Sscanf(result.Balance, "%f", &bal)
	return bal, nil
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

	sigType := 0
	if os.Getenv("POLY_SIGNATURE_TYPE") == "1" {
		sigType = 1
	}

	creds, err = deriveOrFetchCredentials(key, signerAddr, sigType)
	if err != nil {
		return nil, "", "", nil, fmt.Errorf("derive API credentials: %w", err)
	}
	creds.funderAddress = funderAddr

	return key, signerAddr, funderAddr, creds, nil
}

