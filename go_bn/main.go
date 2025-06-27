package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

// 配置常量
const (
	CoinName          = "XRP"  // 交易币种
	ContractType      = "USDC" // 合约类型：USDT 或 USDC
	GridSpacing       = 0.001  // 网格间距 (0.3%)
	InitialQuantity   = 3.0    // 初始交易数量 (币数量)
	Leverage          = 20     // 杠杆倍数
	PositionThreshold = 500.0  // 锁仓阈值
	PositionLimit     = 100.0  // 持仓数量阈值
	SyncTime          = 10     // 同步时间（秒）
	OrderFirstTime    = 10     // 首单间隔时间
)

// initLogger 初始化日志配置
func initLogger() error {
	// 获取当前执行文件的名称(不带扩展名)
	execName := filepath.Base(os.Args[0])
	execName = execName[:len(execName)-len(filepath.Ext(execName))]

	// 创建日志目录
	if err := os.MkdirAll("log", 0755); err != nil {
		return fmt.Errorf("创建日志目录失败: %v", err)
	}

	// 打开日志文件
	logFile, err := os.OpenFile(
		fmt.Sprintf("log/%s.log", execName),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND,
		0666,
	)
	if err != nil {
		return fmt.Errorf("打开日志文件失败: %v", err)
	}

	// 设置日志输出到文件和控制台
	log.SetOutput(io.MultiWriter(os.Stdout, logFile))

	// 设置日志格式
	log.SetFlags(log.Ldate | log.Ltime | log.Lmicroseconds)

	return nil
}

// BotStats 机器人运行统计信息
type BotStats struct {
	StartTime           time.Time
	TotalOrders         int64
	SuccessfulOrders    int64
	FailedOrders        int64
	TotalTrades         int64
	TotalVolume         float64
	LastUpdateTime      time.Time
	WebSocketReconnects int
	APIErrors           int64
	sync.RWMutex
}

// GridTradingBot 网格交易机器人结构体
type GridTradingBot struct {
	client                 *futures.Client
	symbol                 string
	pricePrecision         int
	quantityPrecision      int
	minOrderQuantity       float64
	lock                   sync.Mutex
	longPosition           float64
	shortPosition          float64
	buyLongOrders          float64
	sellLongOrders         float64
	sellShortOrders        float64
	buyShortOrders         float64
	lastLongOrderTime      int64
	lastShortOrderTime     int64
	latestPrice            float64
	bestBidPrice           float64
	bestAskPrice           float64
	longInitialQty         float64
	shortInitialQty        float64
	midPriceLong           float64
	lowerPriceLong         float64
	upperPriceLong         float64
	midPriceShort          float64
	lowerPriceShort        float64
	upperPriceShort        float64
	lastPositionUpdateTime int64
	lastOrdersUpdateTime   int64
	lastTickerUpdateTime   int64
	stats                  *BotStats
}

// NewGridTradingBot 创建新的网格交易机器人实例
func NewGridTradingBot(apiKey, apiSecret string) (*GridTradingBot, error) {
	client := futures.NewClient(apiKey, apiSecret)
	symbol := fmt.Sprintf("%s%s", CoinName, ContractType)

	bot := &GridTradingBot{
		client:             client,
		symbol:             symbol,
		longInitialQty:     InitialQuantity,
		shortInitialQty:    InitialQuantity,
		lastLongOrderTime:  time.Now().Unix(),
		lastShortOrderTime: time.Now().Unix(),
		stats:              &BotStats{StartTime: time.Now()},
	}

	// 初始化精度和最小下单量
	if err := bot.initializeSymbolInfo(); err != nil {
		return nil, fmt.Errorf("初始化交易对信息失败: %v", err)
	}

	// 设置杠杆
	if err := bot.setLeverage(); err != nil {
		return nil, fmt.Errorf("设置杠杆失败: %v", err)
	}

	// 检查并启用对冲模式
	if err := bot.checkAndEnableHedgeMode(); err != nil {
		return nil, fmt.Errorf("启用对冲模式失败: %v", err)
	}

	return bot, nil
}

// initializeSymbolInfo 初始化交易对信息
func (bot *GridTradingBot) initializeSymbolInfo() error {
	exchangeInfo, err := bot.client.NewExchangeInfoService().Do(context.Background())
	if err != nil {
		return fmt.Errorf("获取交易所信息失败: %v", err)
	}

	for _, symbol := range exchangeInfo.Symbols {
		if symbol.Symbol == bot.symbol {
			bot.pricePrecision = symbol.PricePrecision
			bot.quantityPrecision = symbol.QuantityPrecision

			// 获取最小下单量和其他限制
			for _, filter := range symbol.Filters {
				switch filter["filterType"].(string) {
				case "LOT_SIZE":
					minQty, err := strconv.ParseFloat(filter["minQty"].(string), 64)
					if err != nil {
						return fmt.Errorf("解析最小下单量失败: %v", err)
					}
					bot.minOrderQuantity = minQty

				case "PRICE_FILTER":
					// 获取价格步长
					tickSize, err := strconv.ParseFloat(filter["tickSize"].(string), 64)
					if err != nil {
						return fmt.Errorf("解析价格步长失败: %v", err)
					}
					// 计算价格精度
					if tickSize != 0 {
						bot.pricePrecision = int(math.Round(-math.Log10(tickSize)))
					}

				case "MARKET_LOT_SIZE":
					// 市价单的限制
					maxQty, err := strconv.ParseFloat(filter["maxQty"].(string), 64)
					if err != nil {
						return fmt.Errorf("解析最大下单量失败: %v", err)
					}
					log.Printf("市价单最大下单量: %.8f", maxQty)
				}
			}

			log.Printf("交易对 %s 信息初始化完成: 价格精度=%d, 数量精度=%d, 最小下单量=%.8f",
				bot.symbol, bot.pricePrecision, bot.quantityPrecision, bot.minOrderQuantity)
			return nil
		}
	}

	return fmt.Errorf("未找到交易对 %s 的信息", bot.symbol)
}

// roundToStep 将数值四舍五入到指定精度
func (bot *GridTradingBot) roundToStep(value float64, precision int) float64 {
	step := math.Pow(10, float64(-precision))
	return math.Round(value/step) * step
}

// setLeverage 设置杠杆倍数
func (bot *GridTradingBot) setLeverage() error {
	_, err := bot.client.NewChangeLeverageService().
		Symbol(bot.symbol).
		Leverage(Leverage).
		Do(context.Background())
	return err
}

// checkAndEnableHedgeMode 检查并启用对冲模式
func (bot *GridTradingBot) checkAndEnableHedgeMode() error {
	// 获取当前持仓模式
	positionMode, err := bot.client.NewGetPositionModeService().Do(context.Background())
	if err != nil {
		return fmt.Errorf("获取持仓模式失败: %v", err)
	}

	// 如果不是对冲模式，则启用对冲模式
	if !positionMode.DualSidePosition {
		err := bot.client.NewChangePositionModeService().DualSide(true).Do(context.Background())
		if err != nil {
			return fmt.Errorf("启用对冲模式失败: %v", err)
		}
		log.Println("已成功启用对冲模式")
	} else {
		log.Println("当前已是对冲模式")
	}
	return nil
}

// updateStats 更新统计信息
func (bot *GridTradingBot) updateStats(orderSuccess bool, tradeVolume float64) {
	bot.stats.Lock()
	defer bot.stats.Unlock()

	bot.stats.TotalOrders++
	if orderSuccess {
		bot.stats.SuccessfulOrders++
		bot.stats.TotalTrades++
		bot.stats.TotalVolume += tradeVolume
	} else {
		bot.stats.FailedOrders++
	}
	bot.stats.LastUpdateTime = time.Now()
}

// logStats 记录统计信息
func (bot *GridTradingBot) logStats() {
	bot.stats.RLock()
	defer bot.stats.RUnlock()

	uptime := time.Since(bot.stats.StartTime)
	successRate := float64(bot.stats.SuccessfulOrders) / float64(bot.stats.TotalOrders) * 100

	log.Printf("机器人运行统计:\n"+
		"运行时间: %v\n"+
		"总订单数: %d\n"+
		"成功订单: %d\n"+
		"失败订单: %d\n"+
		"成功率: %.2f%%\n"+
		"总成交量: %.4f\n"+
		"WebSocket重连次数: %d\n"+
		"API错误次数: %d",
		uptime,
		bot.stats.TotalOrders,
		bot.stats.SuccessfulOrders,
		bot.stats.FailedOrders,
		successRate,
		bot.stats.TotalVolume,
		bot.stats.WebSocketReconnects,
		bot.stats.APIErrors,
	)
}

// startStatsMonitor 启动统计信息监控
func (bot *GridTradingBot) startStatsMonitor() {
	ticker := time.NewTicker(time.Hour)
	go func() {
		for range ticker.C {
			bot.logStats()
		}
	}()
}

func main() {
	// 初始化日志
	if err := initLogger(); err != nil {
		log.Fatal(err)
	}

	// 从环境变量获取API密钥
	apiKey := os.Getenv("BINANCE_API_KEY")
	apiSecret := os.Getenv("BINANCE_API_SECRET")

	if apiKey == "" || apiSecret == "" {
		log.Fatal("请设置 BINANCE_API_KEY 和 BINANCE_API_SECRET 环境变量")
	}

	// 创建网格交易机器人实例
	bot, err := NewGridTradingBot(apiKey, apiSecret)
	if err != nil {
		log.Fatalf("创建网格交易机器人失败: %v", err)
	}

	// 启动机器人
	if err := bot.Run(); err != nil {
		log.Fatalf("运行网格交易机器人失败: %v", err)
	}
}
