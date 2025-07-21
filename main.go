package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/adshao/go-binance/v2"
	"github.com/joho/godotenv"
)

// TrackedAsset 代表一个我们正在监控的币种状态
type TrackedAsset struct {
	Symbol     string  `json:"symbol"`
	Status     string  `json:"status"` // "breakout" 或 "breakdown"
	EventPrice float64 `json:"eventPrice"`
	EventDate  string  `json:"eventDate"`
}

const STATE_FILE = "state.json"

var (
	DINGTALK_WEBHOOK_URL string
	binanceClient        *binance.Client
	trackedAssets        map[string]TrackedAsset // 用于存储状态的内存变量
)

// loadStateFromFile 从 JSON 文件加载状态到内存
func loadStateFromFile() error {
	trackedAssets = make(map[string]TrackedAsset)
	data, err := os.ReadFile(STATE_FILE)
	if err != nil {
		if os.IsNotExist(err) {
			log.Println("状态文件不存在，将创建一个新的。")
			return nil // 文件不存在是正常情况，直接返回
		}
		return fmt.Errorf("读取状态文件失败: %w", err)
	}
	if len(data) == 0 {
		return nil // 空文件
	}
	err = json.Unmarshal(data, &trackedAssets)
	if err != nil {
		return fmt.Errorf("解析状态文件JSON失败: %w", err)
	}
	log.Printf("成功从 %s 加载 %d 个币种的状态。", STATE_FILE, len(trackedAssets))
	return nil
}

// saveStateToFile 将内存中的状态保存到 JSON 文件
func saveStateToFile() error {
	data, err := json.MarshalIndent(trackedAssets, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化状态到JSON失败: %w", err)
	}
	err = os.WriteFile(STATE_FILE, data, 0644)
	if err != nil {
		return fmt.Errorf("写入状态文件失败: %w", err)
	}
	log.Printf("成功将 %d 个币种的状态保存到 %s。", len(trackedAssets), STATE_FILE)
	return nil
}

func main() {
	if err := godotenv.Load(); err != nil {
		log.Fatal("错误: 无法加载 .env 文件。")
	}

	apiKey := os.Getenv("BINANCE_API_KEY")
	secretKey := os.Getenv("BINANCE_SECRET_KEY")
	binanceClient = binance.NewClient(apiKey, secretKey)
	DINGTALK_WEBHOOK_URL = os.Getenv("DINGTALK_WEBHOOK_URL")

	log.Println("程序启动，执行首次即时检测...")
	runCheck()

	log.Println("首次检测完成。启动每日定时任务...")
	go schedule()

	select {}
}

func schedule() {
	now := time.Now()
	next := time.Date(now.Year(), now.Month(), now.Day(), 8, 0, 0, 0, now.Location())
	if now.After(next) {
		next = next.Add(24 * time.Hour)
	}
	log.Printf("下一次自动检测时间: %s\n", next.Format("2006-01-02 15:04:05"))
	<-time.After(next.Sub(now))

	for {
		runCheck()
		<-time.After(24 * time.Hour)
	}
}

// runCheck 是执行所有逻辑的主函数
func runCheck() {
	log.Println("开始新一轮检测...")
	// 1. 加载状态
	if err := loadStateFromFile(); err != nil {
		log.Printf("严重错误: 加载状态失败: %v", err)
		return
	}

	// 2. 分析和追踪
	dailyBreakouts, dailyBreakdowns, trackedGains, trackedLosses := trackAndAnalyze()

	// 3. 发送报告
	sendFourPartDingTalkMessage(dailyBreakouts, dailyBreakdowns, trackedGains, trackedLosses)

	// 4. 保存状态
	if err := saveStateToFile(); err != nil {
		log.Printf("严重错误: 保存状态失败: %v", err)
	}
	log.Println("本轮检测完成。")
}

// trackAndAnalyze 核心分析逻辑
func trackAndAnalyze() (dailyBreakouts, dailyBreakdowns, trackedGains, trackedLosses []string) {
	symbols, err := getAllUSDTSymbols()
	if err != nil {
		log.Printf("错误: 获取交易对列表失败: %v", err)
		return
	}

	for _, s := range symbols {
		klines, err := binanceClient.NewKlinesService().Symbol(s).Interval("1d").Limit(61).Do(context.Background())
		if err != nil || len(klines) < 61 {
			continue
		}

		var sum float64
		for i := 0; i < 60; i++ {
			p, _ := strconv.ParseFloat(klines[i].Close, 64)
			sum += p
		}
		ma60 := sum / 60

		previousClose, _ := strconv.ParseFloat(klines[59].Close, 64)
		latestClose, _ := strconv.ParseFloat(klines[60].Close, 64)

		isNewBreakout := latestClose > ma60 && previousClose <= ma60
		isNewBreakdown := latestClose < ma60 && previousClose >= ma60

		// 情况一：当日新突破
		if isNewBreakout {
			report := fmt.Sprintf("%s (突破价: %f)", s, latestClose)
			dailyBreakouts = append(dailyBreakouts, report)
			trackedAssets[s] = TrackedAsset{
				Symbol:     s,
				Status:     "breakout",
				EventPrice: latestClose,
				EventDate:  time.Now().Format("2006-01-02"),
			}
			continue // 处理完当日事件后，跳过追踪逻辑
		}

		// 情况二：当日新跌破
		if isNewBreakdown {
			report := fmt.Sprintf("%s (跌破价: %f)", s, latestClose)
			dailyBreakdowns = append(dailyBreakdowns, report)
			trackedAssets[s] = TrackedAsset{
				Symbol:     s,
				Status:     "breakdown",
				EventPrice: latestClose,
				EventDate:  time.Now().Format("2006-01-02"),
			}
			continue // 处理完当日事件后，跳过追踪逻辑
		}

		// 情况三：追踪历史状态
		if asset, ok := trackedAssets[s]; ok {
			// 追踪已突破的币种
			if asset.Status == "breakout" && latestClose > ma60 {
				gain := (latestClose - asset.EventPrice) / asset.EventPrice * 100
				report := fmt.Sprintf("%s (从 %f 至今涨幅: %.2f%%)", s, asset.EventPrice, gain)
				trackedGains = append(trackedGains, report)
			}
			// 追踪已跌破的币种
			if asset.Status == "breakdown" && latestClose < ma60 {
				loss := (asset.EventPrice - latestClose) / asset.EventPrice * 100
				report := fmt.Sprintf("%s (从 %f 至今跌幅: %.2f%%)", s, asset.EventPrice, loss)
				trackedLosses = append(trackedLosses, report)
			}
		}
	}
	return
}

// sendFourPartDingTalkMessage 发送包含四部分的钉钉消息
func sendFourPartDingTalkMessage(dailyBreakouts, dailyBreakdowns, trackedGains, trackedLosses []string) {
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("### MA60 均线监控日报 (%s)\n\n", time.Now().Format("2006-01-02")))

	formatSection := func(title string, items []string) {
		builder.WriteString(fmt.Sprintf("**%s**\n\n", title))
		if len(items) > 0 {
			for _, item := range items {
				builder.WriteString(fmt.Sprintf("- %s\n", item))
			}
		} else {
			builder.WriteString("- 无\n")
		}
		builder.WriteString("\n")
	}

	formatSection("🚀 当日突破 (MA60)", dailyBreakouts)
	formatSection("🚨 当日跌破 (MA60)", dailyBreakdowns)
	formatSection("📈 已突破币种追踪", trackedGains)
	formatSection("📉 已跌破币种追踪", trackedLosses)

	// ... (发送HTTP请求的代码与之前版本相同)
	dingTalkMsg := DingTalkMessage{
		MsgType:  "markdown",
		Markdown: DingTalkMarkdown{Title: "MA60 均线监控", Text: builder.String()},
	}
	jsonData, _ := json.Marshal(dingTalkMsg)
	req, _ := http.NewRequest("POST", DINGTALK_WEBHOOK_URL, bytes.NewBuffer(jsonData))
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("错误: 发送钉钉消息失败: %v", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	log.Printf("钉钉消息发送成功, 响应: %s", string(body))
}

// --- 辅助函数 (与之前版本相同) ---

func getAllUSDTSymbols() ([]string, error) {
	// ...
	exchangeInfo, err := binanceClient.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return nil, err
	}
	var usdtSymbols []string
	for _, s := range exchangeInfo.Symbols {
		if s.QuoteAsset == "USDT" && s.Status == "TRADING" && s.IsSpotTradingAllowed {
			usdtSymbols = append(usdtSymbols, s.Symbol)
		}
	}
	return usdtSymbols, nil
}

type DingTalkMessage struct {
	MsgType  string           `json:"msgtype"`
	Markdown DingTalkMarkdown `json:"markdown"`
}
type DingTalkMarkdown struct {
	Title string `json:"title"`
	Text  string `json:"text"`
}
