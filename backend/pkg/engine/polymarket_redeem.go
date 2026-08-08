package engine

// Live settlement redemption for hold-to-redemption (S2) positions.
//
// Paper mode resolves settlements with local bookkeeping (ResolvePosition).
// Live mode must actually collect the USDC on-chain: winning conditional
// tokens sit in the wallet until someone calls ConditionalTokens
// .redeemPositions. Nothing here runs unless POLY_LIVE=true.
//
// Wallet plumbing: with a Magic.link account (POLY_SIGNATURE_TYPE=1) the
// tokens live in the CREATE2 proxy wallet (funderAddress), not the EOA. The
// proxy factory exposes proxy(Call[]) which executes calls FROM the caller's
// derived proxy wallet — so the EOA signs a normal transaction to the factory,
// and the factory routes the redeem call through the proxy that owns the
// tokens. For plain EOA accounts (no funder) we call the CTF directly.
//
// Gas: unlike CLOB orders (relayer-sponsored), this is a raw on-chain
// transaction — the EOA pays gas in POL. A redemption costs ~150-250k gas,
// well under $0.01 at typical Polygon prices, but the EOA must hold a little
// POL or every redemption will fail with "insufficient funds" (surfaced as a
// CRITICAL log; the position stays OPEN so a manual redeem is still possible).

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"

	"tiq/backend/pkg/db"
)

const (
	// Canonical Polygon mainnet addresses (same ones the CLOB order flow signs
	// against — see polymarket_live.go for the exchange side).
	ctfAddress          = "0x4D97DCd97eC945f40cF65F87097ACe5EA0476045" // Gnosis ConditionalTokens
	usdcAddress         = "0x2791Bca1f2de4661ED88A30C99A7a9449Aa84174" // USDC.e (Polymarket collateral)
	proxyFactoryAddress = "0xaB45c5A4B0c941a2F231C04C3f49182e1A254052" // Polymarket Magic proxy factory

	polygonChainID = 137

	// defaultPolygonRPC is used when POLYGON_RPC_URL isn't set. Public keyless
	// endpoint; fine for a handful of redemption calls per day. (polygon-rpc.com
	// now 401s keyless requests — verified publicnode works from this machine.)
	defaultPolygonRPC = "https://polygon-bor-rpc.publicnode.com"

	// How long to keep polling for on-chain resolution after expiry before
	// giving up (position stays OPEN; a later tick restarts the wait).
	redemptionResolveWait = 15 * time.Minute
	// Minimum spacing between full redemption attempts for one position.
	redemptionRetrySpacing = 30 * time.Second

	// How long to wait for Polymarket's own auto-redemption (their relayer
	// credits winnings gaslessly — same flow as the website, where users never
	// pay POL) before falling back to a self-submitted on-chain redemption.
	autoRedeemWait = 5 * time.Minute
)

// ABI fragments for the two contracts we touch. payoutNumerators is the public
// array getter (bytes32, index) the CTF auto-generates.
const ctfABIJSON = `[
 {"name":"payoutDenominator","type":"function","stateMutability":"view","inputs":[{"name":"conditionId","type":"bytes32"}],"outputs":[{"name":"","type":"uint256"}]},
 {"name":"payoutNumerators","type":"function","stateMutability":"view","inputs":[{"name":"conditionId","type":"bytes32"},{"name":"index","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]},
 {"name":"redeemPositions","type":"function","stateMutability":"nonpayable","inputs":[{"name":"collateralToken","type":"address"},{"name":"parentCollectionId","type":"bytes32"},{"name":"conditionId","type":"bytes32"},{"name":"indexSets","type":"uint256[]"}],"outputs":[]}
]`

const proxyFactoryABIJSON = `[
 {"name":"proxy","type":"function","stateMutability":"payable","inputs":[{"name":"calls","type":"tuple[]","components":[
   {"name":"typeCode","type":"uint8"},
   {"name":"to","type":"address"},
   {"name":"value","type":"uint256"},
   {"name":"data","type":"bytes"}
 ]}],"outputs":[{"name":"returnValues","type":"bytes[]"}]}
]`

// proxyCall mirrors the factory's Call struct (typeCode 1 = CALL).
type proxyCall struct {
	TypeCode uint8
	To       common.Address
	Value    *big.Int
	Data     []byte
}

var (
	ctfABI          abi.ABI
	proxyFactoryABI abi.ABI
	abiInitOnce     sync.Once
	abiInitErr      error
)

func initRedeemABIs() error {
	abiInitOnce.Do(func() {
		var err error
		ctfABI, err = abi.JSON(strings.NewReader(ctfABIJSON))
		if err != nil {
			abiInitErr = fmt.Errorf("ctf abi: %w", err)
			return
		}
		proxyFactoryABI, err = abi.JSON(strings.NewReader(proxyFactoryABIJSON))
		if err != nil {
			abiInitErr = fmt.Errorf("proxy factory abi: %w", err)
		}
	})
	return abiInitErr
}

func polygonRPCURL() string {
	if v := os.Getenv("POLYGON_RPC_URL"); v != "" {
		return v
	}
	return defaultPolygonRPC
}

// conditionResolution reads the CTF payout vector for a condition. resolved is
// false until the oracle has reported. yesWon reflects outcome index 0 (the
// "Up"/YES token — same ordering as clobTokenIds and our yesTokenID).
func conditionResolution(ctx context.Context, client *ethclient.Client, conditionID common.Hash) (resolved bool, yesWon bool, err error) {
	if err := initRedeemABIs(); err != nil {
		return false, false, err
	}
	ctf := common.HexToAddress(ctfAddress)

	callView := func(method string, args ...interface{}) (*big.Int, error) {
		data, err := ctfABI.Pack(method, args...)
		if err != nil {
			return nil, fmt.Errorf("pack %s: %w", method, err)
		}
		out, err := client.CallContract(ctx, callMsg(ctf, data), nil)
		if err != nil {
			return nil, fmt.Errorf("call %s: %w", method, err)
		}
		vals, err := ctfABI.Unpack(method, out)
		if err != nil {
			return nil, fmt.Errorf("unpack %s: %w", method, err)
		}
		return vals[0].(*big.Int), nil
	}

	den, err := callView("payoutDenominator", conditionID)
	if err != nil {
		return false, false, err
	}
	if den.Sign() == 0 {
		return false, false, nil // oracle hasn't reported yet
	}
	num0, err := callView("payoutNumerators", conditionID, big.NewInt(0))
	if err != nil {
		return false, false, err
	}
	return true, num0.Sign() > 0, nil
}

// redeemCalldata builds the CTF redeemPositions call for both binary index
// sets ([1,2] = outcome 0 and outcome 1). The CTF pays out whatever the caller
// actually holds, so redeeming both sides in one call is safe and sidesteps
// any index-mapping mistake costing money.
func redeemCalldata(conditionID common.Hash) ([]byte, error) {
	if err := initRedeemABIs(); err != nil {
		return nil, err
	}
	return ctfABI.Pack("redeemPositions",
		common.HexToAddress(usdcAddress),
		[32]byte{}, // parentCollectionId: root collection
		[32]byte(conditionID),
		[]*big.Int{big.NewInt(1), big.NewInt(2)},
	)
}

// wrapForProxy wraps CTF calldata in a factory proxy() call so it executes
// from the Magic proxy wallet that owns the tokens.
func wrapForProxy(ctfData []byte) (to common.Address, data []byte, err error) {
	if err := initRedeemABIs(); err != nil {
		return common.Address{}, nil, err
	}
	calls := []proxyCall{{
		TypeCode: 1, // CALL
		To:       common.HexToAddress(ctfAddress),
		Value:    big.NewInt(0),
		Data:     ctfData,
	}}
	packed, err := proxyFactoryABI.Pack("proxy", calls)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("pack proxy(): %w", err)
	}
	return common.HexToAddress(proxyFactoryAddress), packed, nil
}

// LiveRedeemPosition drives one expired hold-to-redemption position through
// on-chain settlement: wait for the oracle, determine the authoritative
// outcome, send the redeem transaction for winners, then hand off to
// ResolvePosition for bookkeeping. Losing positions skip the transaction
// (redeeming worthless tokens burns gas for nothing) and resolve at $0.
//
// Runs in its own goroutine per position; startLiveRedemption dedupes.
func (p *PolymarketEngine) LiveRedeemPosition(pos db.Position) {
	defer p.finishRedemption(pos.ID)

	condHex := conditionID(pos.Instrument)
	if !strings.HasPrefix(condHex, "0x") || len(condHex) != 66 {
		p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: instrument %q has no parsable condition ID — cannot redeem on-chain.", pos.ID, pos.Instrument))
		return
	}
	cond := common.HexToHash(condHex)

	ctx, cancel := context.WithTimeout(context.Background(), redemptionResolveWait)
	defer cancel()

	client, err := ethclient.DialContext(ctx, polygonRPCURL())
	if err != nil {
		p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: Polygon RPC dial failed: %v (POLYGON_RPC_URL=%q). Position stays OPEN; will retry.", pos.ID, err, polygonRPCURL()))
		return
	}
	defer client.Close()

	// 1. Wait for the oracle to report. These 5-min crypto markets usually
	// resolve within a minute of expiry.
	var yesWon bool
	for {
		resolved, yw, err := conditionResolution(ctx, client, cond)
		if err != nil {
			p.store.Log("WARN", fmt.Sprintf("[Live Redeem] %s: resolution check failed (%v) — retrying.", pos.ID, err))
		} else if resolved {
			yesWon = yw
			break
		}
		select {
		case <-ctx.Done():
			p.store.Log("WARN", fmt.Sprintf("[Live Redeem] %s: condition still unresolved after %s — giving up for now, position stays OPEN.", pos.ID, redemptionResolveWait))
			return
		case <-time.After(10 * time.Second):
		}
	}

	weWon := (pos.Units > 0 && yesWon) || (pos.Units < 0 && !yesWon)
	p.store.Log("INFO", fmt.Sprintf("[Live Redeem] %s: on-chain resolution is %s — our %s side %s.",
		pos.ID, map[bool]string{true: "YES/Up", false: "NO/Down"}[yesWon],
		map[bool]string{true: "YES", false: "NO"}[pos.Units > 0],
		map[bool]string{true: "WON", false: "lost"}[weWon]))

	if !weWon {
		// Nothing to collect; book the $0 settlement.
		if err := p.ResolvePosition(pos.ID, 0.0); err != nil {
			p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: ResolvePosition(0) failed: %v", pos.ID, err))
		}
		return
	}

	// 2. Gasless path first: Polymarket's relayer normally auto-redeems winning
	// positions (the same flow that makes the website gasless — confirmed in
	// this account's own activity history, where REDEEM entries appeared with
	// no fee and no gas paid). Watch the public activity feed for our REDEEM;
	// if it shows up, the USDC is already in the proxy wallet and no
	// transaction of ours is needed.
	if p.waitForAutoRedemption(ctx, condHex, pos.OpenTime) {
		p.store.Log("INFO", fmt.Sprintf("[Live Redeem] %s: Polymarket auto-redeemed (gasless). Booking settlement at $1.00.", pos.ID))
		if err := p.ResolvePosition(pos.ID, 1.0); err != nil {
			p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: ResolvePosition(1.0) failed after auto-redeem: %v — bookkeeping out of sync, fix manually.", pos.ID, err))
		}
		return
	}
	p.store.Log("WARN", fmt.Sprintf("[Live Redeem] %s: no auto-redemption after %s — falling back to self-submitted on-chain redeem (needs POL in signer EOA).", pos.ID, autoRedeemWait))

	// 3. Fallback: build and send the redeem transaction ourselves.
	ctfData, err := redeemCalldata(cond)
	if err != nil {
		p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: calldata build failed: %v", pos.ID, err))
		return
	}
	to := common.HexToAddress(ctfAddress)
	data := ctfData
	if p.funderAddress != "" {
		// Magic proxy account: route through the factory so the proxy wallet
		// (which owns the tokens) executes the redeem.
		to, data, err = wrapForProxy(ctfData)
		if err != nil {
			p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: proxy wrap failed: %v", pos.ID, err))
			return
		}
	}

	from := common.HexToAddress(p.walletAddress)

	// 4. Gas preflight: a clear "fund the EOA" message beats a cryptic RPC error.
	if bal, err := client.BalanceAt(ctx, from, nil); err == nil {
		// 300k gas at 200 gwei = 0.06 POL; require a bit above one worst-case tx.
		minWei := new(big.Int).Mul(big.NewInt(300000*200), big.NewInt(1_000_000_000))
		if bal.Cmp(minWei) < 0 {
			p.store.Log("CRITICAL", fmt.Sprintf("[Live Redeem] %s: signer EOA %s has %.4f POL — not enough for redemption gas. Send ~0.1 POL to that address; position stays OPEN and will retry.",
				pos.ID, p.walletAddress, weiToPOL(bal)))
			return
		}
	}

	nonce, err := client.PendingNonceAt(ctx, from)
	if err != nil {
		p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: nonce fetch failed: %v", pos.ID, err))
		return
	}
	gasPrice, err := client.SuggestGasPrice(ctx)
	if err != nil {
		p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: gas price fetch failed: %v", pos.ID, err))
		return
	}
	// Pad 25% so the tx doesn't sit underpriced during a gas spike.
	gasPrice = new(big.Int).Div(new(big.Int).Mul(gasPrice, big.NewInt(125)), big.NewInt(100))

	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		To:       &to,
		Value:    big.NewInt(0),
		Gas:      300000,
		GasPrice: gasPrice,
		Data:     data,
	})
	signed, err := types.SignTx(tx, types.LatestSignerForChainID(big.NewInt(polygonChainID)), p.privateKey)
	if err != nil {
		p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: tx signing failed: %v", pos.ID, err))
		return
	}
	if err := client.SendTransaction(ctx, signed); err != nil {
		p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: tx broadcast failed: %v. Position stays OPEN; will retry.", pos.ID, err))
		return
	}
	txHash := signed.Hash()
	p.store.Log("INFO", fmt.Sprintf("[Live Redeem] %s: redemption tx sent: %s (gas price %.1f gwei).", pos.ID, txHash.Hex(), weiToGwei(gasPrice)))

	// 5. Wait for the receipt; only credit the balance on confirmed success.
	for {
		receipt, err := client.TransactionReceipt(ctx, txHash)
		if err == nil && receipt != nil {
			if receipt.Status == types.ReceiptStatusSuccessful {
				p.store.Log("INFO", fmt.Sprintf("[Live Redeem] %s: redemption CONFIRMED in block %d (tx %s). Booking settlement at $1.00.", pos.ID, receipt.BlockNumber.Uint64(), txHash.Hex()))
				if err := p.ResolvePosition(pos.ID, 1.0); err != nil {
					p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: ResolvePosition(1.0) failed after confirmed redeem: %v — bookkeeping out of sync, fix manually.", pos.ID, err))
				}
			} else {
				p.store.Log("ERROR", fmt.Sprintf("[Live Redeem] %s: redemption tx REVERTED (tx %s). Position stays OPEN; will retry.", pos.ID, txHash.Hex()))
			}
			return
		}
		select {
		case <-ctx.Done():
			p.store.Log("WARN", fmt.Sprintf("[Live Redeem] %s: no receipt for %s before timeout — it may still confirm; position stays OPEN pending a retry (double-redeem is harmless: second tx redeems zero tokens).", pos.ID, txHash.Hex()))
			return
		case <-time.After(3 * time.Second):
		}
	}
}

// waitForAutoRedemption polls Polymarket's public activity feed for a REDEEM
// entry on this condition, which is how their relayer's gasless auto-redemption
// shows up (validated earlier against this account's real July 4 history).
// Returns true once seen. Entries are matched on condition ID and must be
// newer than the position open, so an old redemption can't false-positive.
func (p *PolymarketEngine) waitForAutoRedemption(ctx context.Context, condHex string, openedAfter time.Time) bool {
	deadline := time.Now().Add(autoRedeemWait)
	for time.Now().Before(deadline) {
		if p.checkActivityForRedeem(condHex, openedAfter) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(20 * time.Second):
		}
	}
	return false
}

// checkActivityForRedeem does one poll of the activity feed. Any error just
// reads as "not seen yet" — the caller keeps polling until its deadline.
func (p *PolymarketEngine) checkActivityForRedeem(condHex string, openedAfter time.Time) bool {
	url := fmt.Sprintf("https://data-api.polymarket.com/activity?user=%s&limit=100", p.accountKey())
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var entries []struct {
		Type        string  `json:"type"`
		ConditionID string  `json:"conditionId"`
		Timestamp   int64   `json:"timestamp"`
		UsdcSize    float64 `json:"usdcSize"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return false
	}
	for _, e := range entries {
		if strings.EqualFold(e.Type, "REDEEM") &&
			strings.EqualFold(e.ConditionID, condHex) &&
			e.Timestamp >= openedAfter.Unix() {
			p.store.Log("INFO", fmt.Sprintf("[Live Redeem] auto-redemption seen in activity feed: $%.4f USDC credited for condition %s.", e.UsdcSize, condHex[:10]+"..."))
			return true
		}
	}
	return false
}

// startLiveRedemption launches LiveRedeemPosition once per position with
// spacing between attempts. Returns immediately; safe to call every tick.
func (p *PolymarketEngine) startLiveRedemption(pos db.Position) {
	p.redeemMu.Lock()
	last, seen := p.redeemAttempts[pos.ID]
	if seen && time.Since(last) < redemptionRetrySpacing {
		p.redeemMu.Unlock()
		return
	}
	p.redeemAttempts[pos.ID] = time.Now()
	p.redeemMu.Unlock()
	go p.LiveRedeemPosition(pos)
}

// finishRedemption releases the in-flight marker so a failed attempt can retry
// after redemptionRetrySpacing. Successful attempts close the position, so the
// expiry branch never re-fires for them; the marker is pruned with the others.
func (p *PolymarketEngine) finishRedemption(posID string) {
	p.redeemMu.Lock()
	// Keep the timestamp (spacing) but drop entries once they age out.
	for id, t := range p.redeemAttempts {
		if time.Since(t) > time.Hour {
			delete(p.redeemAttempts, id)
		}
	}
	_ = posID
	p.redeemMu.Unlock()
}

func callMsg(to common.Address, data []byte) ethereum.CallMsg {
	return ethereum.CallMsg{To: &to, Data: data}
}

func weiToPOL(wei *big.Int) float64 {
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e18)).Float64()
	return f
}

func weiToGwei(wei *big.Int) float64 {
	f, _ := new(big.Float).Quo(new(big.Float).SetInt(wei), big.NewFloat(1e9)).Float64()
	return f
}
