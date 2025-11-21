package trader

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// OKXTrader 欧易交易器
type OKXTrader struct {
	apiKey     string
	secretKey  string
	passphrase string
	testnet    bool
	baseURL    string
	client     *http.Client

	// 余额缓存
	cachedBalance     map[string]interface{}
	balanceCacheTime  time.Time
	balanceCacheMutex sync.RWMutex

	// 持仓缓存
	cachedPositions     []map[string]interface{}
	positionsCacheTime  time.Time
	positionsCacheMutex sync.RWMutex

	// 缓存有效期（15秒）
	cacheDuration time.Duration
}

// NewOKXTrader 创建欧易交易器
func NewOKXTrader(apiKey, secretKey, passphrase string, testnet bool) *OKXTrader {
	// 验证 API 密钥格式
	if apiKey == "" || secretKey == "" || passphrase == "" {
		log.Printf("⚠️  警告: OKX API 密钥、密钥或密码短语为空")
	} else if len(apiKey) < 32 || len(secretKey) < 32 {
		log.Printf("⚠️  警告: OKX API 密钥格式可能不正确")
		log.Printf("   API Key 长度: %d, Secret Key 长度: %d", len(apiKey), len(secretKey))
	}

	return &OKXTrader{
		apiKey:        apiKey,
		secretKey:     secretKey,
		passphrase:    passphrase,
		testnet:       testnet,
		baseURL:       "https://www.okx.com",
		client:        &http.Client{Timeout: 30 * time.Second},
		cacheDuration: 15 * time.Second,
	}
}

// signRequest 生成 OKX API 签名
func (t *OKXTrader) signRequest(method, path, body string, timestamp string) string {
	message := timestamp + method + path + body
	mac := hmac.New(sha256.New, []byte(t.secretKey))
	mac.Write([]byte(message))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// doRequest 执行 HTTP 请求
func (t *OKXTrader) doRequest(method, endpoint, body string) ([]byte, error) {
	timestamp := time.Now().UTC().Format("2006-01-02T15:04:05.000Z")

	// 生成签名
	signature := t.signRequest(method, endpoint, body, timestamp)

	// 创建请求
	reqURL := t.baseURL + endpoint
	req, err := http.NewRequest(method, reqURL, bytes.NewBufferString(body))
	if err != nil {
		return nil, err
	}

	// 设置请求头
	req.Header.Set("OK-ACCESS-KEY", t.apiKey)
	req.Header.Set("OK-ACCESS-SIGN", signature)
	req.Header.Set("OK-ACCESS-TIMESTAMP", timestamp)
	req.Header.Set("OK-ACCESS-PASSPHRASE", t.passphrase)
	// OKX 模拟盘：x-simulated-trading: 1
	if t.testnet {
		req.Header.Set("x-simulated-trading", "1")
	}
	req.Header.Set("Content-Type", "application/json")

	// 发送请求
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	// 检查响应状态
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	// 解析响应
	var apiResp struct {
		Code string          `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}

	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("解析响应失败: %w", err)
	}

	if apiResp.Code != "0" {
		return nil, fmt.Errorf("API错误: %s - %s", apiResp.Code, apiResp.Msg)
	}

	return apiResp.Data, nil
}

// GetBalance 获取账户余额（带缓存）
func (t *OKXTrader) GetBalance() (map[string]interface{}, error) {
	// 先检查缓存是否有效
	t.balanceCacheMutex.RLock()
	if t.cachedBalance != nil && time.Since(t.balanceCacheTime) < t.cacheDuration {
		cacheAge := time.Since(t.balanceCacheTime)
		t.balanceCacheMutex.RUnlock()
		log.Printf("✓ 使用缓存的账户余额（缓存时间: %.1f秒前）", cacheAge.Seconds())
		return t.cachedBalance, nil
	}
	t.balanceCacheMutex.RUnlock()

	// 缓存过期或不存在，调用API
	log.Printf("🔄 缓存过期，正在调用OKX API获取账户余额...")
	data, err := t.doRequest("GET", "/api/v5/account/balance", "")
	if err != nil {
		log.Printf("❌ OKX API调用失败: %v", err)
		return nil, fmt.Errorf("获取账户信息失败: %w", err)
	}

	// 解析余额数据
	var balanceData []struct {
		Details []struct {
			Currency string `json:"ccy"`
			Balance  string `json:"bal"`
			AvailBal string `json:"availBal"`
			Frozen   string `json:"frozenBal"`
		} `json:"details"`
		TotalEq  string `json:"totalEq"`  // 总权益
		AvailEq  string `json:"availEq"`  // 可用权益
		MgnRatio string `json:"mgnRatio"` // 保证金率
	}

	if err := json.Unmarshal(data, &balanceData); err != nil {
		return nil, fmt.Errorf("解析余额数据失败: %w", err)
	}

	if len(balanceData) == 0 {
		return nil, fmt.Errorf("未找到账户余额数据")
	}

	result := make(map[string]interface{})
	result["totalWalletBalance"], _ = strconv.ParseFloat(balanceData[0].TotalEq, 64)
	result["availableBalance"], _ = strconv.ParseFloat(balanceData[0].AvailEq, 64)
	result["totalUnrealizedProfit"] = 0.0 // OKX 需要从持仓计算

	log.Printf("✓ OKX API返回: 总余额=%s, 可用=%s",
		balanceData[0].TotalEq,
		balanceData[0].AvailEq)

	// 更新缓存
	t.balanceCacheMutex.Lock()
	t.cachedBalance = result
	t.balanceCacheTime = time.Now()
	t.balanceCacheMutex.Unlock()

	return result, nil
}

// GetPositions 获取所有持仓（带缓存）
func (t *OKXTrader) GetPositions() ([]map[string]interface{}, error) {
	// 先检查缓存是否有效
	t.positionsCacheMutex.RLock()
	if t.cachedPositions != nil && time.Since(t.positionsCacheTime) < t.cacheDuration {
		cacheAge := time.Since(t.positionsCacheTime)
		t.positionsCacheMutex.RUnlock()
		log.Printf("✓ 使用缓存的持仓信息（缓存时间: %.1f秒前）", cacheAge.Seconds())
		return t.cachedPositions, nil
	}
	t.positionsCacheMutex.RUnlock()

	// 缓存过期或不存在，调用API
	log.Printf("🔄 缓存过期，正在调用OKX API获取持仓信息...")
	data, err := t.doRequest("GET", "/api/v5/account/positions", "")
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	// 解析持仓数据
	var positions []struct {
		InstID      string `json:"instId"`      // 交易对
		Pos         string `json:"pos"`         // 持仓数量
		AvgPx       string `json:"avgPx"`       // 开仓均价
		MarkPx      string `json:"markPx"`      // 标记价格
		Upl         string `json:"upl"`         // 未实现盈亏
		Lever       string `json:"lever"`       // 杠杆倍数
		LiqPx       string `json:"liqPx"`       // 预估强平价格
		PosSide     string `json:"posSide"`     // 持仓方向: net, long, short
		MgnMode     string `json:"mgnMode"`     // 保证金模式: isolated, cross
		NotionalUsd string `json:"notionalUsd"` // 持仓价值（USD）
	}

	if err := json.Unmarshal(data, &positions); err != nil {
		return nil, fmt.Errorf("解析持仓数据失败: %w", err)
	}

	var result []map[string]interface{}
	for _, pos := range positions {
		posAmt, _ := strconv.ParseFloat(pos.Pos, 64)
		if posAmt == 0 {
			continue // 跳过无持仓的
		}

		posMap := make(map[string]interface{})
		posMap["symbol"] = pos.InstID
		posMap["entryPrice"], _ = strconv.ParseFloat(pos.AvgPx, 64)
		posMap["markPrice"], _ = strconv.ParseFloat(pos.MarkPx, 64)
		posMap["unRealizedProfit"], _ = strconv.ParseFloat(pos.Upl, 64)
		posMap["leverage"], _ = strconv.ParseFloat(pos.Lever, 64)
		posMap["liquidationPrice"], _ = strconv.ParseFloat(pos.LiqPx, 64)

		// 判断方向并统一将数量转为正数
		// OKX API: posSide 为 "long" 或 "net" 且 pos > 0 表示多仓，否则为空仓
		// pos 字段在空仓时可能是负数（net模式）或正数（short模式）
		if pos.PosSide == "long" || (pos.PosSide == "net" && posAmt > 0) {
			posMap["side"] = "long"
			posMap["positionAmt"] = posAmt // 多仓已经是正数
		} else {
			posMap["side"] = "short"
			// 统一转为正数：如果 posSide 是 "short" 可能已经是正数，如果是 "net" 且为负数则需要取绝对值
			if posAmt < 0 {
				posMap["positionAmt"] = -posAmt
			} else {
				posMap["positionAmt"] = posAmt
			}
		}

		result = append(result, posMap)
	}

	// 更新缓存
	t.positionsCacheMutex.Lock()
	t.cachedPositions = result
	t.positionsCacheTime = time.Now()
	t.positionsCacheMutex.Unlock()

	return result, nil
}

// SetMarginMode 设置仓位模式
func (t *OKXTrader) SetMarginMode(symbol string, isCrossMargin bool) error {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	marginMode := "isolated"
	if isCrossMargin {
		marginMode = "cross"
	}

	body := map[string]interface{}{
		"instId":  okxSymbol,
		"mgnMode": marginMode,
	}

	bodyJSON, _ := json.Marshal(body)
	_, err := t.doRequest("POST", "/api/v5/account/set-position-mode", string(bodyJSON))

	marginModeStr := "全仓"
	if !isCrossMargin {
		marginModeStr = "逐仓"
	}

	if err != nil {
		log.Printf("  ⚠️ 设置仓位模式失败: %v", err)
		return nil // 不返回错误，让交易继续
	}

	log.Printf("  ✓ %s 仓位模式已设置为 %s", symbol, marginModeStr)
	return nil
}

// SetLeverage 设置杠杆
func (t *OKXTrader) SetLeverage(symbol string, leverage int) error {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	body := map[string]interface{}{
		"instId": okxSymbol,
		"lever":  strconv.Itoa(leverage),
	}

	bodyJSON, _ := json.Marshal(body)
	_, err := t.doRequest("POST", "/api/v5/account/set-leverage", string(bodyJSON))

	if err != nil {
		log.Printf("  ⚠️ 设置杠杆失败: %v", err)
		return fmt.Errorf("设置杠杆失败: %w", err)
	}

	log.Printf("  ✓ %s 杠杆已切换为 %dx", symbol, leverage)
	time.Sleep(2 * time.Second) // 等待冷却期

	return nil
}

// OpenLong 开多仓
func (t *OKXTrader) OpenLong(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	// 先取消该币种的所有委托单
	if err := t.CancelAllOrders(okxSymbol); err != nil {
		log.Printf("  ⚠ 取消旧委托单失败（可能没有委托单）: %v", err)
	}

	// 设置杠杆
	if err := t.SetLeverage(okxSymbol, leverage); err != nil {
		log.Printf("  ⚠ 设置杠杆失败: %v", err)
	}

	// 格式化数量
	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// 下单
	body := map[string]interface{}{
		"instId":  okxSymbol,
		"tdMode":  "isolated", // 或 "cross"
		"side":    "buy",
		"ordType": "market",
		"sz":      quantityStr,
	}

	bodyJSON, _ := json.Marshal(body)
	data, err := t.doRequest("POST", "/api/v5/trade/order", string(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("开多仓失败: %w", err)
	}

	var orderResp []struct {
		OrdId   string `json:"ordId"`
		ClOrdId string `json:"clOrdId"`
		Tag     string `json:"tag"`
		SMsg    string `json:"sMsg"`
	}

	if err := json.Unmarshal(data, &orderResp); err != nil {
		return nil, fmt.Errorf("解析订单响应失败: %w", err)
	}

	if len(orderResp) == 0 {
		return nil, fmt.Errorf("订单响应为空")
	}

	result := map[string]interface{}{
		"orderId":  orderResp[0].OrdId,
		"symbol":   symbol,
		"side":     "buy",
		"quantity": quantityStr,
	}

	log.Printf("  ✓ 开多仓成功: %s %s", symbol, quantityStr)
	return result, nil
}

// OpenShort 开空仓
func (t *OKXTrader) OpenShort(symbol string, quantity float64, leverage int) (map[string]interface{}, error) {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	// 先取消该币种的所有委托单
	if err := t.CancelAllOrders(okxSymbol); err != nil {
		log.Printf("  ⚠ 取消旧委托单失败（可能没有委托单）: %v", err)
	}

	// 设置杠杆
	if err := t.SetLeverage(okxSymbol, leverage); err != nil {
		log.Printf("  ⚠ 设置杠杆失败: %v", err)
	}

	// 格式化数量
	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// 下单
	body := map[string]interface{}{
		"instId":  okxSymbol,
		"tdMode":  "isolated",
		"side":    "sell",
		"ordType": "market",
		"sz":      quantityStr,
	}

	bodyJSON, _ := json.Marshal(body)
	data, err := t.doRequest("POST", "/api/v5/trade/order", string(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("开空仓失败: %w", err)
	}

	var orderResp []struct {
		OrdId   string `json:"ordId"`
		ClOrdId string `json:"clOrdId"`
		Tag     string `json:"tag"`
		SMsg    string `json:"sMsg"`
	}

	if err := json.Unmarshal(data, &orderResp); err != nil {
		return nil, fmt.Errorf("解析订单响应失败: %w", err)
	}

	if len(orderResp) == 0 {
		return nil, fmt.Errorf("订单响应为空")
	}

	result := map[string]interface{}{
		"orderId":  orderResp[0].OrdId,
		"symbol":   symbol,
		"side":     "sell",
		"quantity": quantityStr,
	}

	log.Printf("  ✓ 开空仓成功: %s %s", symbol, quantityStr)
	return result, nil
}

// CloseLong 平多仓
func (t *OKXTrader) CloseLong(symbol string, quantity float64) (map[string]interface{}, error) {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	// 获取当前持仓
	positions, err := t.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	var currentPos float64
	for _, pos := range positions {
		posSymbol := pos["symbol"].(string)
		// 支持两种格式匹配
		if (posSymbol == symbol || posSymbol == okxSymbol) && pos["side"] == "long" {
			currentPos = pos["positionAmt"].(float64)
			break
		}
	}

	if currentPos == 0 {
		return nil, fmt.Errorf("没有多仓持仓")
	}

	// 如果 quantity=0，平掉全部
	if quantity == 0 || quantity > currentPos {
		quantity = currentPos
	}

	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// 平仓（卖出）
	body := map[string]interface{}{
		"instId":  okxSymbol,
		"tdMode":  "isolated",
		"side":    "sell",
		"posSide": "long",
		"ordType": "market",
		"sz":      quantityStr,
	}

	bodyJSON, _ := json.Marshal(body)
	data, err := t.doRequest("POST", "/api/v5/trade/order", string(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("平多仓失败: %w", err)
	}

	var orderResp []struct {
		OrdId string `json:"ordId"`
	}

	if err := json.Unmarshal(data, &orderResp); err != nil {
		return nil, fmt.Errorf("解析订单响应失败: %w", err)
	}

	result := map[string]interface{}{
		"orderId":  orderResp[0].OrdId,
		"symbol":   symbol,
		"side":     "sell",
		"quantity": quantityStr,
	}

	log.Printf("  ✓ 平多仓成功: %s %s", symbol, quantityStr)
	return result, nil
}

// CloseShort 平空仓
func (t *OKXTrader) CloseShort(symbol string, quantity float64) (map[string]interface{}, error) {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	// 获取当前持仓
	positions, err := t.GetPositions()
	if err != nil {
		return nil, fmt.Errorf("获取持仓失败: %w", err)
	}

	var currentPos float64
	for _, pos := range positions {
		posSymbol := pos["symbol"].(string)
		// 支持两种格式匹配
		if (posSymbol == symbol || posSymbol == okxSymbol) && pos["side"] == "short" {
			// GetPositions 已经将空仓数量转为正数
			currentPos = pos["positionAmt"].(float64)
			break
		}
	}

	if currentPos == 0 {
		return nil, fmt.Errorf("没有空仓持仓")
	}

	// 如果 quantity=0，平掉全部
	if quantity == 0 || quantity > currentPos {
		quantity = currentPos
	}

	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return nil, err
	}

	// 平仓（买入）
	body := map[string]interface{}{
		"instId":  okxSymbol,
		"tdMode":  "isolated",
		"side":    "buy",
		"posSide": "short",
		"ordType": "market",
		"sz":      quantityStr,
	}

	bodyJSON, _ := json.Marshal(body)
	data, err := t.doRequest("POST", "/api/v5/trade/order", string(bodyJSON))
	if err != nil {
		return nil, fmt.Errorf("平空仓失败: %w, %s", err, string(bodyJSON))
	}

	var orderResp []struct {
		OrdId   string `json:"ordId"`
		ClOrdId string `json:"clOrdId"`
		SMsg    string `json:"sMsg"`
		SCode   string `json:"sCode"`
	}

	if err := json.Unmarshal(data, &orderResp); err != nil {
		return nil, fmt.Errorf("解析订单响应失败: %w", err)
	}

	if len(orderResp) == 0 {
		return nil, fmt.Errorf("平空仓失败: 订单响应为空，可能数量不正确或订单已取消")
	}

	// 检查订单是否成功（sCode 为 "0" 表示成功）
	if orderResp[0].SCode != "" && orderResp[0].SCode != "0" {
		errorMsg := orderResp[0].SMsg
		if errorMsg == "" {
			errorMsg = "订单失败"
		}
		return nil, fmt.Errorf("平空仓失败: %s (错误码: %s)", errorMsg, orderResp[0].SCode)
	}

	result := map[string]interface{}{
		"orderId":  orderResp[0].OrdId,
		"symbol":   symbol,
		"side":     "buy",
		"quantity": quantityStr,
	}

	log.Printf("  ✓ 平空仓成功: %s %s", symbol, quantityStr)
	return result, nil
}

// GetMarketPrice 获取市场价格
func (t *OKXTrader) GetMarketPrice(symbol string) (float64, error) {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)
	endpoint := "/api/v5/market/ticker?instId=" + url.QueryEscape(okxSymbol)
	data, err := t.doRequest("GET", endpoint, "")
	if err != nil {
		return 0, fmt.Errorf("获取市场价格失败: %w", err)
	}

	var tickerData []struct {
		Last string `json:"last"`
	}

	if err := json.Unmarshal(data, &tickerData); err != nil {
		return 0, fmt.Errorf("解析价格数据失败: %w", err)
	}

	if len(tickerData) == 0 {
		return 0, fmt.Errorf("未找到价格数据")
	}

	price, err := strconv.ParseFloat(tickerData[0].Last, 64)
	if err != nil {
		return 0, fmt.Errorf("解析价格失败: %w", err)
	}

	return price, nil
}

// SetStopLoss 设置止损单
func (t *OKXTrader) SetStopLoss(symbol string, positionSide string, quantity, stopPrice float64) error {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return err
	}

	stopPriceStr := strconv.FormatFloat(stopPrice, 'f', -1, 64)

	side := "sell"
	if positionSide == "short" {
		side = "buy"
	}

	body := map[string]interface{}{
		"instId":          okxSymbol,
		"tdMode":          "isolated",
		"side":            side,
		"ordType":         "conditional",
		"sz":              quantityStr,
		"slTriggerPx":     stopPriceStr,
		"slTriggerPxType": "last",
	}

	bodyJSON, _ := json.Marshal(body)
	_, err = t.doRequest("POST", "/api/v5/trade/order", string(bodyJSON))
	if err != nil {
		log.Printf("  ⚠️ 设置止损单失败: %v", err)
		return nil // 不返回错误
	}

	log.Printf("  ✓ 设置止损单成功: %s @ %s", symbol, stopPriceStr)
	return nil
}

// SetTakeProfit 设置止盈单
func (t *OKXTrader) SetTakeProfit(symbol string, positionSide string, quantity, takeProfitPrice float64) error {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	quantityStr, err := t.FormatQuantity(symbol, quantity)
	if err != nil {
		return err
	}

	takeProfitStr := strconv.FormatFloat(takeProfitPrice, 'f', -1, 64)

	side := "sell"
	if positionSide == "short" {
		side = "buy"
	}

	body := map[string]interface{}{
		"instId":          okxSymbol,
		"tdMode":          "isolated",
		"side":            side,
		"ordType":         "conditional",
		"sz":              quantityStr,
		"tpTriggerPx":     takeProfitStr,
		"tpTriggerPxType": "last",
	}

	bodyJSON, _ := json.Marshal(body)
	_, err = t.doRequest("POST", "/api/v5/trade/order", string(bodyJSON))
	if err != nil {
		log.Printf("  ⚠️ 设置止盈单失败: %v", err)
		return nil // 不返回错误
	}

	log.Printf("  ✓ 设置止盈单成功: %s @ %s", symbol, takeProfitStr)
	return nil
}

// CancelAllOrders 取消该币种的所有挂单
func (t *OKXTrader) CancelAllOrders(symbol string) error {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	body := map[string]interface{}{
		"instId": okxSymbol,
	}

	bodyJSON, _ := json.Marshal(body)
	_, err := t.doRequest("POST", "/api/v5/trade/cancel-all-after", string(bodyJSON))
	if err != nil {
		// 如果没有挂单，不报错
		if strings.Contains(err.Error(), "no order") {
			return nil
		}
		return err
	}

	log.Printf("  ✓ 取消 %s 的所有挂单", symbol)
	return nil
}

// convertSymbolToOKX 将币安格式的交易对转换为 OKX 格式
// 例如: "BTCUSDT" -> "BTC-USDT-SWAP"
func convertSymbolToOKX(symbol string) string {
	// 去掉 USDT 后缀
	if len(symbol) > 4 && symbol[len(symbol)-4:] == "USDT" {
		base := symbol[:len(symbol)-4]
		return fmt.Sprintf("%s-USDT-SWAP", base)
	}
	// 如果已经是 OKX 格式，直接返回
	if strings.Contains(symbol, "-") {
		return symbol
	}
	// 默认添加 -USDT-SWAP
	return fmt.Sprintf("%s-USDT-SWAP", symbol)
}

// FormatQuantity 格式化数量到正确的精度
func (t *OKXTrader) FormatQuantity(symbol string, quantity float64) (string, error) {
	// 转换交易对格式
	okxSymbol := convertSymbolToOKX(symbol)

	// 获取交易对信息
	endpoint := "/api/v5/public/instruments?instType=SWAP&instId=" + url.QueryEscape(okxSymbol)
	data, err := t.doRequest("GET", endpoint, "")
	if err != nil {
		// 如果获取失败，使用默认精度
		return strconv.FormatFloat(quantity, 'f', 4, 64), nil
	}

	var instruments []struct {
		LotSz string `json:"lotSz"` // 下单数量精度
		MinSz string `json:"minSz"` // 最小下单数量
	}

	if err := json.Unmarshal(data, &instruments); err != nil || len(instruments) == 0 {
		return strconv.FormatFloat(quantity, 'f', 4, 64), nil
	}

	// 解析精度
	lotSz, _ := strconv.ParseFloat(instruments[0].LotSz, 64)
	if lotSz > 0 {
		// 根据精度格式化
		precision := 0
		if lotSz < 1 {
			precision = len(strings.TrimRight(fmt.Sprintf("%.10f", lotSz), "0")) - 2
		}
		return strconv.FormatFloat(quantity, 'f', precision, 64), nil
	}

	return strconv.FormatFloat(quantity, 'f', 4, 64), nil
}
