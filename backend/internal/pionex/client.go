package pionex

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

const (
	DefaultHost    = "https://api.pionex.com"
	RequestTimeout = 15 * time.Second
)

// Client is a Pionex-only REST client. Mutating requests are never retried
// automatically because a transport failure can leave their remote outcome unknown.
type Client struct {
	baseURL        string
	signer         *Signer
	httpClient     *http.Client
	publicLimiter  *RateLimiter
	privateLimiter *RateLimiter
	clock          *Clock
	now            func() time.Time
}

func NewClient(baseURL string, apiKey, apiSecret string) *Client {
	return NewClientWithHTTPClient(baseURL, apiKey, apiSecret, &http.Client{Timeout: RequestTimeout})
}

// NewClientWithHTTPClient exists for contract tests and controlled transports.
func NewClientWithHTTPClient(baseURL, apiKey, apiSecret string, httpClient *http.Client) *Client {
	if baseURL == "" {
		baseURL = DefaultHost
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: RequestTimeout}
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		signer:     NewSigner(apiKey, apiSecret),
		httpClient: httpClient,
		// Pionex documents a global 10 req/s per IP limit for all public
		// endpoints; the previous 20/s burst guaranteed 429 blackouts.
		publicLimiter:  NewRateLimiter(10, 10),
		privateLimiter: NewRateLimiter(10, 10),
		clock:          NewClock(),
		now:            time.Now,
	}
}

type APIEnvelope[T any] struct {
	Result    bool   `json:"result"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	Timestamp int64  `json:"timestamp"`
	Data      T      `json:"data"`
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
	// OutcomeUnknown is true only when a mutating request was sent but no
	// authoritative response was received.
	OutcomeUnknown bool
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("pionex API error [%s]: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("pionex HTTP error [%d]: %s", e.StatusCode, e.Message)
}

func IsOutcomeUnknown(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.OutcomeUnknown
}

type FuturesBalance struct {
	Coin   string          `json:"coin"`
	Free   decimal.Decimal `json:"free"`
	Frozen decimal.Decimal `json:"frozen"`
	Debts  decimal.Decimal `json:"debts"`
}

// BUOrderData follows the official Futures Grid create contract exactly.
type BUOrderData struct {
	Top                 decimal.Decimal  `json:"top"`
	Bottom              decimal.Decimal  `json:"bottom"`
	Row                 int              `json:"row"`
	GridType            string           `json:"grid_type"`
	Trend               string           `json:"trend"`
	Leverage            int              `json:"leverage"`
	ExtraMargin         *decimal.Decimal `json:"extraMargin,omitempty"`
	QuoteInvestment     decimal.Decimal  `json:"quoteInvestment"`
	LossStopType        string           `json:"lossStopType,omitempty"`
	LossStop            *decimal.Decimal `json:"lossStop,omitempty"`
	ProfitStopType      string           `json:"profitStopType,omitempty"`
	ProfitStop          *decimal.Decimal `json:"profitStop,omitempty"`
	InvestCoin          string           `json:"investCoin,omitempty"`
	InvestmentFrom      string           `json:"investmentFrom,omitempty"`
	MovingIndicatorType string           `json:"movingIndicatorType,omitempty"`
	EnableFollowClosed  bool             `json:"enableFollowClosed,omitempty"`
}

type NativeFuturesGridCreateParams struct {
	Base        string      `json:"base"`
	Quote       string      `json:"quote"`
	BUOrderData BUOrderData `json:"buOrderData"`
}

type NativeFuturesGridCreateResult struct {
	BUOrderID string `json:"buOrderId"`
}

type FuturesGridOrder struct {
	BUOrderID   string              `json:"buOrderId"`
	Base        string              `json:"base"`
	Quote       string              `json:"quote"`
	Status      string              `json:"status"`
	ReasonBy    string              `json:"reasonBy"`
	BUOrderData BUOrderDataResponse `json:"buOrderData"`
}

type BUOrderDataResponse struct {
	Status               string          `json:"status"`
	ReasonBy             string          `json:"reasonBy"`
	TopRaw               json.RawMessage `json:"top"`
	BottomRaw            json.RawMessage `json:"bottom"`
	RowRaw               json.RawMessage `json:"row"`
	GridType             string          `json:"gridType"`
	Trend                string          `json:"trend"`
	LeverageRaw          json.RawMessage `json:"leverage"`
	QuoteInvestmentRaw   json.RawMessage `json:"quoteInvestment"`
	InvestmentAliasRaw   json.RawMessage `json:"investment"`
	OpenPriceRaw         json.RawMessage `json:"openPrice"`
	PositionRaw          json.RawMessage `json:"position"`
	PositionOpenPriceRaw json.RawMessage `json:"positionOpenPrice"`
	ProfitWithdrawnRaw   json.RawMessage `json:"profitWithdrawn"`
	TotalProfitRaw       json.RawMessage `json:"totalProfit"`
	ProfitRaw            json.RawMessage `json:"profit"`
	PnlRaw               json.RawMessage `json:"pnl"`
	RealizedProfitRaw    json.RawMessage `json:"realizedProfit"`
	GridProfitRaw        json.RawMessage `json:"gridProfit"`
	ProfitReduceRaw      json.RawMessage `json:"profitReduce"`
	ProfitExitedRaw      json.RawMessage `json:"profitExited"`
	FundingFeePaymentRaw json.RawMessage `json:"fundingFeePayment"`
	ClosedBaseAmountRaw  json.RawMessage `json:"closedBaseAmount"`
	RiskStatus           string          `json:"riskStatus"`
	LiquidationPriceRaw  json.RawMessage `json:"liquidationPrice"`

	Top               decimal.Decimal `json:"-"`
	Bottom            decimal.Decimal `json:"-"`
	Row               int             `json:"-"`
	Leverage          int             `json:"-"`
	QuoteInvestment   decimal.Decimal `json:"-"`
	OpenPrice         decimal.Decimal `json:"-"`
	Position          decimal.Decimal `json:"-"`
	PositionOpenPrice decimal.Decimal `json:"-"`
	ProfitWithdrawn   decimal.Decimal `json:"-"`
	TotalProfit       decimal.Decimal `json:"-"`
	ClosedBaseAmount  decimal.Decimal `json:"-"`
	LiquidationPrice  decimal.Decimal `json:"-"`
}

func parseDecimalRaw(raw json.RawMessage) decimal.Decimal {
	if len(raw) == 0 {
		return decimal.Zero
	}
	s := strings.Trim(string(raw), "\" \t\n\r")
	if s == "" || s == "null" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

func parseIntRaw(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	s := strings.Trim(string(raw), "\" \t\n\r")
	if s == "" || s == "null" {
		return 0
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return int(f)
	}
	return 0
}

func (b *BUOrderDataResponse) UnmarshalJSON(data []byte) error {
	type Alias BUOrderDataResponse
	var aux Alias
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*b = BUOrderDataResponse(aux)
	b.Top = parseDecimalRaw(b.TopRaw)
	b.Bottom = parseDecimalRaw(b.BottomRaw)
	b.Row = parseIntRaw(b.RowRaw)
	b.Leverage = parseIntRaw(b.LeverageRaw)
	b.QuoteInvestment = parseDecimalRaw(b.QuoteInvestmentRaw)
	if !rawFieldPresent(b.QuoteInvestmentRaw) {
		b.QuoteInvestment = parseDecimalRaw(b.InvestmentAliasRaw)
	}
	b.OpenPrice = parseDecimalRaw(b.OpenPriceRaw)
	b.Position = parseDecimalRaw(b.PositionRaw)
	b.PositionOpenPrice = parseDecimalRaw(b.PositionOpenPriceRaw)
	b.ProfitWithdrawn = parseDecimalRaw(b.ProfitWithdrawnRaw)
	b.TotalProfit = parseDecimalRaw(b.TotalProfitRaw)
	if b.TotalProfit.IsZero() {
		b.TotalProfit = parseDecimalRaw(b.ProfitRaw)
	}
	if b.TotalProfit.IsZero() {
		b.TotalProfit = parseDecimalRaw(b.PnlRaw)
	}
	if b.TotalProfit.IsZero() {
		b.TotalProfit = parseDecimalRaw(b.RealizedProfitRaw)
	}
	if b.ProfitWithdrawn.IsZero() && !b.TotalProfit.IsZero() {
		b.ProfitWithdrawn = b.TotalProfit
	}
	b.ClosedBaseAmount = parseDecimalRaw(b.ClosedBaseAmountRaw)
	b.LiquidationPrice = parseDecimalRaw(b.LiquidationPriceRaw)
	return nil
}

// GridProfit returns the accumulated realized grid profit — the "Grid Profit"
// figure the Pionex app shows for a running futures grid. The documented
// carrier is buOrderData.profitReduce ("grid profit from position reduction,
// accumulated"); gridProfit is kept as an observed-legacy fallback.
// profitWithdrawn is deliberately NOT consulted: it stays 0 for a running
// grid (profit compounds inside the bot unless manually released), so the
// old mapping silently zeroed realized PnL for every live bot.
func (b *BUOrderDataResponse) GridProfit() decimal.Decimal {
	if b == nil {
		return decimal.Zero
	}
	if value := parseDecimalRaw(b.ProfitReduceRaw); !value.IsZero() {
		return value
	}
	return parseDecimalRaw(b.GridProfitRaw)
}

// FundingFeePayment returns the exchange-reported per-bot cumulative funding
// (negative when paid, positive when received). It is strictly per-bot truth
// and must replace any symbol-wide history accumulation when present.
func (b *BUOrderDataResponse) FundingFeePayment() decimal.Decimal {
	if b == nil {
		return decimal.Zero
	}
	return parseDecimalRaw(b.FundingFeePaymentRaw)
}

// FundingFeePaymentReported distinguishes "exchange sent 0 funding" from
// "field absent": only a present field may resync local funding columns.
func (b *BUOrderDataResponse) FundingFeePaymentReported() bool {
	if b == nil {
		return false
	}
	return rawFieldPresent(b.FundingFeePaymentRaw)
}

// Investment returns the exchange-reported quote investment of the grid —
// the remote truth an invest_in reconcile must resync to. Absent on payloads
// that predate the field; callers must treat !ok as "unknown", never zero.
func (b *BUOrderDataResponse) Investment() (decimal.Decimal, bool) {
	if b == nil {
		return decimal.Zero, false
	}
	if rawFieldPresent(b.QuoteInvestmentRaw) || rawFieldPresent(b.InvestmentAliasRaw) {
		return b.QuoteInvestment, true
	}
	return decimal.Zero, false
}

// FinalProfitSource names the chain leg that produced a settled figure, so
// callers can refuse chain legs that are provably incomplete for their close
// class (v2.0.75: a native stop-loss showed +0.22 from the grid-only leg
// while the app's Total PnL was −2.5 — the position-close leg was silently
// dropped).
type FinalProfitSource string

const (
	// FinalProfitExited is the documented settled figure ("Exited profit") —
	// the only field the exchange itself nets grid + position-close + fees in.
	FinalProfitExited FinalProfitSource = "profit_exited"
	// FinalProfitTotalAlias covers totalProfit/profit/pnl/realizedProfit —
	// observed variants of the full-total carrier on finished records.
	FinalProfitTotalAlias FinalProfitSource = "total_profit_alias"
	// FinalProfitGridFlat is grid + funding on a provably flat position: with
	// no residual inventory there IS no position-close leg, so the sum is the
	// complete total.
	FinalProfitGridFlat FinalProfitSource = "grid_funding_flat"
	// FinalProfitGridResidual is grid + funding while the record still shows
	// inventory (position ≠ 0 or closedBaseAmount ≠ 0): the position-close
	// PnL is NOT included. On a loss-class close this leg is a lie by
	// construction and callers must gate it.
	FinalProfitGridResidual FinalProfitSource = "grid_funding_residual"
	// FinalProfitWithdrawn is the withdrawn-profit carrier.
	FinalProfitWithdrawn FinalProfitSource = "profit_withdrawn"
	// FinalProfitNone means no carrier carried anything.
	FinalProfitNone FinalProfitSource = "none"
)

// SettledProfit returns the finished grid's settled total plus the chain leg
// it came from. Priority: the exchange's own netted figures first (profitExited,
// total-alias), then grid+funding — complete only when the record shows no
// residual inventory — and withdrawn profit last.
func (b *BUOrderDataResponse) SettledProfit() (decimal.Decimal, FinalProfitSource) {
	if b == nil {
		return decimal.Zero, FinalProfitNone
	}
	if exited := parseDecimalRaw(b.ProfitExitedRaw); !exited.IsZero() {
		return exited, FinalProfitExited
	}
	if total := b.TotalProfit; !total.IsZero() {
		return total, FinalProfitTotalAlias
	}
	gridPlusFunding := b.GridProfit().Add(b.FundingFeePayment())
	if !gridPlusFunding.IsZero() {
		if !b.Position.IsZero() || !b.ClosedBaseAmount.IsZero() {
			return gridPlusFunding, FinalProfitGridResidual
		}
		return gridPlusFunding, FinalProfitGridFlat
	}
	if withdrawn := parseDecimalRaw(b.ProfitWithdrawnRaw); !withdrawn.IsZero() {
		return withdrawn, FinalProfitWithdrawn
	}
	return decimal.Zero, FinalProfitNone
}

// FinalProfit returns just the settled figure (provenance-free). New callers
// should prefer SettledProfit — the source decides whether the figure is
// trustworthy for the bot's close class.
func (b *BUOrderDataResponse) FinalProfit() decimal.Decimal {
	total, _ := b.SettledProfit()
	return total
}

// rawFieldPresent reports whether a raw JSON payload slot carried an actual
// value (missing key and explicit null both count as absent).
func rawFieldPresent(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	return strings.TrimSpace(string(raw)) != "null"
}

func (f *FuturesGridOrder) UnmarshalJSON(data []byte) error {
	type Alias FuturesGridOrder
	var aux struct {
		Alias
		KeyID   string `json:"keyId"`
		OrderID string `json:"orderId"`
		ID      string `json:"id"`
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	*f = FuturesGridOrder(aux.Alias)
	if f.BUOrderID == "" {
		if aux.OrderID != "" {
			f.BUOrderID = aux.OrderID
		} else if aux.KeyID != "" {
			f.BUOrderID = aux.KeyID
		} else if aux.ID != "" {
			f.BUOrderID = aux.ID
		}
	}
	return nil
}

func (c *Client) CreateFuturesGridBot(
	ctx context.Context,
	params NativeFuturesGridCreateParams,
) (*NativeFuturesGridCreateResult, error) {
	body, err := json.Marshal(params)
	if err != nil {
		return nil, fmt.Errorf("marshal futures grid create request: %w", err)
	}
	var raw json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/create", nil, body, true, 1, &raw); err != nil {
		return nil, err
	}
	result := &NativeFuturesGridCreateResult{BUOrderID: findStringField(raw, "buOrderId")}
	return result, nil
}

func (c *Client) GetFuturesGridBot(ctx context.Context, buOrderID string) (*FuturesGridOrder, error) {
	query := url.Values{"buOrderId": []string{buOrderID}}
	var result FuturesGridOrder
	if err := c.do(ctx, http.MethodGet, "/api/v1/bot/orders/futuresGrid/order", query, nil, true, 1, &result); err != nil {
		return nil, err
	}
	if result.BUOrderID == "" {
		result.BUOrderID = buOrderID
	}
	return &result, nil
}

type CancelFuturesGridParams struct {
	BUOrderID     string `json:"buOrderId,omitempty"`
	CloseNote     string `json:"closeNote,omitempty"`
	CloseSellMode string `json:"closeSellModel,omitempty"`
	Immediate     bool   `json:"immediate"`
	CloseSlippage string `json:"closeSlippage,omitempty"`
}

func (c *Client) CancelFuturesGridBot(ctx context.Context, params CancelFuturesGridParams) error {
	body, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("marshal futures grid cancel request: %w", err)
	}
	return c.do(ctx, http.MethodPost, "/api/v1/bot/orders/futuresGrid/cancel", nil, body, true, 1, nil)
}

func (c *Client) GetFuturesBalances(ctx context.Context) ([]FuturesBalance, error) {
	var data struct {
		Balances []FuturesBalance `json:"balances"`
	}
	if err := c.do(ctx, http.MethodGet, "/uapi/v1/account/balances", nil, nil, true, 5, &data); err != nil {
		return nil, err
	}
	return data.Balances, nil
}

// FuturesDetailBalance is one cross-margin coin row of GET /uapi/v1/account/detail.
// Unlike /uapi/v1/account/balances this schema carries unrealizedPnL and the
// derived totals, which makes it the ONLY wallet source an equity figure can
// be marked against — the grid bots' own profit fields never see the fees.
type FuturesDetailBalance struct {
	Coin               string          `json:"coin"`
	Assets             decimal.Decimal `json:"assets"` // total assets = free + frozen
	Free               decimal.Decimal `json:"free"`   // available balance
	Frozen             decimal.Decimal `json:"frozen"` // frozen (position margin + order frozen)
	Transferable       decimal.Decimal `json:"transferable"`
	Available          decimal.Decimal `json:"available"` // available margin
	UnrealizedPnL      decimal.Decimal `json:"unrealizedPnL"`
	TotalInitialMargin decimal.Decimal `json:"totalInitialMargin"`
	Debts              decimal.Decimal `json:"debts"`
}

// UnmarshalJSON decodes one balance row defensively. The official OpenAPI
// declares every numeric field a string, but the live API has deviated from
// the docs before (v2.0.74: leverage/row arrived as unquoted numbers where
// the spec said strings), and shopspring's decimal rejects an empty string
// outright. A single malformed field must not zero out the whole USDT row —
// that produced the "empty wallet" silent no-op that left the equity ledger
// at 0 rows for weeks (v2.0.75–v2.0.79 prod): string/number/null all decode,
// missing keys stay zero.
func (b *FuturesDetailBalance) UnmarshalJSON(raw []byte) error {
	var probe struct {
		Coin               string          `json:"coin"`
		Assets             json.RawMessage `json:"assets"`
		Free               json.RawMessage `json:"free"`
		Frozen             json.RawMessage `json:"frozen"`
		Transferable       json.RawMessage `json:"transferable"`
		Available          json.RawMessage `json:"available"`
		UnrealizedPnL      json.RawMessage `json:"unrealizedPnL"`
		TotalInitialMargin json.RawMessage `json:"totalInitialMargin"`
		Debts              json.RawMessage `json:"debts"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return err
	}
	b.Coin = probe.Coin
	targets := []*decimal.Decimal{
		&b.Assets, &b.Free, &b.Frozen, &b.Transferable,
		&b.Available, &b.UnrealizedPnL, &b.TotalInitialMargin, &b.Debts,
	}
	sources := []json.RawMessage{
		probe.Assets, probe.Free, probe.Frozen, probe.Transferable,
		probe.Available, probe.UnrealizedPnL, probe.TotalInitialMargin, probe.Debts,
	}
	for i, source := range sources {
		trimmed := strings.TrimSpace(string(source))
		// Missing key, JSON null, and the empty string all mean "no value":
		// the field stays zero instead of failing the whole row.
		if trimmed == "" || trimmed == "null" || trimmed == `""` || trimmed == `''` {
			continue
		}
		if err := json.Unmarshal(source, targets[i]); err != nil {
			return fmt.Errorf("futures balance field %d: %w", i, err)
		}
	}
	return nil
}

// GetFuturesAccountDetail returns the cross-margin balance list of GET
// /uapi/v1/account/detail (official docs: AccountDetail.balances[]).
// The primary decode follows the documented shape data.balances[]; if the
// live payload nests the object one envelope deeper (data.data.balances[]),
// the probe recovers it — an empty result here is reported by the caller as
// an observable EQUITY_CAPTURE_FAILED instead of a silent empty wallet.
func (c *Client) GetFuturesAccountDetail(ctx context.Context) ([]FuturesDetailBalance, error) {
	balances, _, err := c.GetFuturesAccountDetailRaw(ctx)
	return balances, err
}

// GetFuturesAccountDetailRaw additionally returns a bounded snippet of the
// raw payload so an EMPTY_DECODE alert can carry the live shape (the spec
// has deviated three times already: numbers-for-strings, `results` key,
// unknown nesting) instead of a blind "no USDT row".
func (c *Client) GetFuturesAccountDetailRaw(ctx context.Context) ([]FuturesDetailBalance, string, error) {
	raw, err := c.doRaw(ctx, http.MethodGet, "/uapi/v1/account/detail", nil, nil, true, 5)
	if err != nil {
		return nil, "", err
	}
	var data struct {
		Balances []FuturesDetailBalance `json:"balances"`
		Data     struct {
			Balances []FuturesDetailBalance `json:"balances"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, snippet(raw), fmt.Errorf("decode pionex response data: %w", err)
	}
	if len(data.Balances) > 0 {
		return data.Balances, snippet(raw), nil
	}
	return data.Data.Balances, snippet(raw), nil
}

// snippet bounds a raw payload for telemetry: response bodies carry amounts
// only (no credentials), but keep it short and single-line.
func snippet(raw []byte) string {
	s := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, string(raw))
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

// doRaw performs a signed request and returns the undecoded envelope payload
// (the `data` field) so callers can probe undocumented shape deviations.
func (c *Client) doRaw(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
	private bool,
	weight int,
) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := c.do(ctx, method, path, query, body, private, weight, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *Client) do(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
	private bool,
	weight int,
	out any,
) error {
	limiter := c.publicLimiter
	if private {
		limiter = c.privateLimiter
	}
	attempts := 1
	if method == http.MethodGet {
		attempts = 3
	}
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := limiter.Wait(ctx, weight); err != nil {
			return err
		}
		var req *http.Request
		var err error
		if private {
			req, err = c.newSignedRequest(ctx, method, path, query, body)
		} else {
			req, err = c.newPublicRequest(ctx, method, path, query, body)
		}
		if err != nil {
			return err
		}
		resp, err := c.httpClient.Do(req)
		if err != nil {
			if method != http.MethodGet {
				return &APIError{Message: err.Error(), OutcomeUnknown: true}
			}
			if attempt < attempts {
				if err := waitRetry(ctx, attempt); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("pionex request failed: %w", err)
		}
		responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			if method != http.MethodGet {
				return &APIError{StatusCode: resp.StatusCode, Message: readErr.Error(), OutcomeUnknown: true}
			}
			return fmt.Errorf("read pionex response: %w", readErr)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			limiter.TriggerCooldown(60 * time.Second)
			return &APIError{StatusCode: resp.StatusCode, Code: "RATE_LIMITED", Message: "60 second cooldown activated"}
		}
		if resp.StatusCode >= 500 && method == http.MethodGet && attempt < attempts {
			if err := waitRetry(ctx, attempt); err != nil {
				return err
			}
			continue
		}

		var envelope APIEnvelope[json.RawMessage]
		if err := json.Unmarshal(responseBody, &envelope); err != nil {
			return &APIError{
				StatusCode:     resp.StatusCode,
				Message:        "invalid JSON response: " + bodySnippet(responseBody),
				OutcomeUnknown: method != http.MethodGet && resp.StatusCode >= 500,
			}
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || !envelope.Result {
			message := envelope.Message
			if message == "" {
				message = http.StatusText(resp.StatusCode)
			}
			// A rejected timestamp means the local clock drifted past the
			// +/-20s signing window: resync the offset immediately so the
			// next signed request passes, and retry idempotent GETs at once.
			if private && strings.Contains(strings.ToLower(message+" "+envelope.Code), "timestamp") {
				syncCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				if syncErr := c.clock.SyncWithServer(syncCtx, c.baseURL); syncErr == nil {
					cancel()
					if method == http.MethodGet && attempt < attempts {
						if err := waitRetry(ctx, attempt); err != nil {
							return err
						}
						continue
					}
				} else {
					cancel()
				}
			}
			return &APIError{StatusCode: resp.StatusCode, Code: envelope.Code, Message: message}
		}
		if out == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
			return nil
		}
		if raw, ok := out.(*json.RawMessage); ok {
			*raw = append((*raw)[:0], envelope.Data...)
			return nil
		}
		if err := json.Unmarshal(envelope.Data, out); err != nil {
			return fmt.Errorf("decode pionex response data: %w", err)
		}
		return nil
	}
	return errors.New("pionex request retries exhausted")
}

func (c *Client) newPublicRequest(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
) (*http.Request, error) {
	if query == nil {
		query = url.Values{}
	}
	requestURL := c.baseURL + path
	if encoded := query.Encode(); encoded != "" {
		requestURL += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, method, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	return req, nil
}

func (c *Client) newSignedRequest(
	ctx context.Context,
	method, path string,
	query url.Values,
	body []byte,
) (*http.Request, error) {
	if query == nil {
		query = url.Values{}
	}
	timestamp := c.clock.NowMilli()
	signature, err := c.signer.SignRequest(method, path, query, body, timestamp)
	if err != nil {
		return nil, fmt.Errorf("sign pionex request: %w", err)
	}
	signedQuery := cloneValues(query)
	signedQuery.Set("timestamp", strconv.FormatInt(timestamp, 10))
	req, err := c.newPublicRequest(ctx, method, path, signedQuery, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("PIONEX-KEY", c.signer.apiKey)
	req.Header.Set("PIONEX-SIGNATURE", signature)
	return req, nil
}

func cloneValues(values url.Values) url.Values {
	cloned := make(url.Values, len(values))
	for key, items := range values {
		cloned[key] = append([]string(nil), items...)
	}
	return cloned
}

// bodySnippet renders the first bytes of a non-JSON response body so gateway
// HTML error pages (openresty 4xx/5xx) surface their actual reason in logs.
func bodySnippet(body []byte) string {
	const limit = 160
	snippet := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		return r
	}, string(body))
	snippet = strings.TrimSpace(snippet)
	if len(snippet) > limit {
		snippet = snippet[:limit] + "..."
	}
	return snippet
}

func waitRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt*200) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func findStringField(raw json.RawMessage, field string) string {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	var walk func(any) string
	walk = func(current any) string {
		switch item := current.(type) {
		case map[string]any:
			if candidate, ok := item[field].(string); ok && candidate != "" {
				return candidate
			}
			for _, child := range item {
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range item {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	return walk(value)
}
