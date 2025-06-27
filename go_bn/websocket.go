package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

// Run 启动网格交易机器人
func (bot *GridTradingBot) Run() error {
	// 初始化持仓信息
	if err := bot.syncPositions(); err != nil {
		return fmt.Errorf("同步持仓信息失败: %v", err)
	}

	// 初始化订单信息
	if err := bot.syncOrders(); err != nil {
		return fmt.Errorf("同步订单信息失败: %v", err)
	}

	// 启动统计监控
	bot.startStatsMonitor()

	// 启动WebSocket连接
	errHandler := func(err error) {
		log.Printf("WebSocket错误: %v", err)
		bot.stats.Lock()
		bot.stats.APIErrors++
		bot.stats.Unlock()
	}

	// 启动订单监控
	go bot.monitorOrders()

	// 处理账户更新
	go bot.handleUserData(errHandler)

	// 处理行情更新
	go bot.handleMarketData(errHandler)

	// 保持程序运行
	select {}
}

// handleUserData 处理用户数据WebSocket
func (bot *GridTradingBot) handleUserData(errHandler func(error)) {
	maxRetries := 5
	retryDelay := time.Second * 5

	for {
		for retry := 0; retry < maxRetries; retry++ {
			// 获取listenKey
			listenKey, err := bot.client.NewStartUserStreamService().Do(context.Background())
			if err != nil {
				log.Printf("获取listenKey失败(重试 %d/%d): %v", retry+1, maxRetries, err)
				time.Sleep(retryDelay)
				continue
			}

			// 启动WebSocket连接
			wsHandler := func(event *futures.WsUserDataEvent) {
				if err := bot.processUserDataEvent(event); err != nil {
					log.Printf("处理用户数据失败: %v", err)
				}
			}

			doneC, stopC, err := futures.WsUserDataServe(listenKey, wsHandler, errHandler)
			if err != nil {
				log.Printf("启动用户数据WebSocket失败(重试 %d/%d): %v", retry+1, maxRetries, err)
				time.Sleep(retryDelay)
				continue
			}

			// 定期更新listenKey
			go bot.keepAliveListenKey(listenKey, stopC)

			// 等待连接关闭
			<-doneC
			log.Printf("WebSocket连接已关闭，准备重新连接")
			break
		}

		// 如果重试次数用完仍然失败，等待较长时间后重试
		log.Printf("重试次数已用完，等待60秒后重新尝试")
		time.Sleep(time.Second * 60)
	}
}

// processUserDataEvent 处理用户数据事件
func (bot *GridTradingBot) processUserDataEvent(event *futures.WsUserDataEvent) error {
	switch event.Event {
	case "ORDER_TRADE_UPDATE":
		bot.handleOrderUpdate(event.OrderTradeUpdate)
	case "ACCOUNT_UPDATE":
		bot.handleAccountUpdate(event.AccountUpdate)
	default:
		return fmt.Errorf("未知的事件类型: %s", event.Event)
	}
	return nil
}

// handleMarketData 处理市场数据WebSocket
func (bot *GridTradingBot) handleMarketData(errHandler func(error)) {
	maxRetries := 5
	retryDelay := time.Second * 5

	for {
		for retry := 0; retry < maxRetries; retry++ {
			// 订阅BookTicker
			doneC, _, err := futures.WsBookTickerServe(bot.symbol, bot.handleBookTicker, errHandler)
			if err != nil {
				log.Printf("启动行情WebSocket失败(重试 %d/%d): %v", retry+1, maxRetries, err)
				time.Sleep(retryDelay)
				continue
			}

			// 等待连接关闭
			<-doneC
			log.Printf("行情WebSocket连接已关闭，准备重新连接")
			break
		}

		// 如果重试次数用完仍然失败，等待较长时间后重试
		log.Printf("重试次数已用完，等待60秒后重新尝试")
		time.Sleep(time.Second * 60)
	}
}

// handleOrderUpdate 处理订单更新
func (bot *GridTradingBot) handleOrderUpdate(event futures.WsOrderTradeUpdate) {
	bot.lock.Lock()
	defer bot.lock.Unlock()

	// 解析订单数量
	quantity, err := strconv.ParseFloat(event.OriginalQty, 64)
	if err != nil {
		log.Printf("解析订单数量失败: %v", err)
		return
	}

	// 解析已成交数量
	filledQty, err := strconv.ParseFloat(event.AccumulatedFilledQty, 64)
	if err != nil {
		log.Printf("解析已成交数量失败: %v", err)
		return
	}

	remaining := quantity - filledQty

	// 处理订单状态更新
	switch event.Status {
	case futures.OrderStatusTypeNew:
		if event.Side == futures.SideTypeBuy {
			if event.PositionSide == futures.PositionSideTypeLong {
				bot.buyLongOrders += remaining
			} else {
				bot.buyShortOrders += remaining
			}
		} else {
			if event.PositionSide == futures.PositionSideTypeLong {
				bot.sellLongOrders += remaining
			} else {
				bot.sellShortOrders += remaining
			}
		}

	case futures.OrderStatusTypeFilled:
		if event.Side == futures.SideTypeBuy {
			if event.PositionSide == futures.PositionSideTypeLong {
				bot.longPosition += filledQty
				bot.buyLongOrders = math.Max(0, bot.buyLongOrders-filledQty)
			} else {
				bot.shortPosition = math.Max(0, bot.shortPosition-filledQty)
				bot.buyShortOrders = math.Max(0, bot.buyShortOrders-filledQty)
			}
		} else {
			if event.PositionSide == futures.PositionSideTypeLong {
				bot.longPosition = math.Max(0, bot.longPosition-filledQty)
				bot.sellLongOrders = math.Max(0, bot.sellLongOrders-filledQty)
			} else {
				bot.shortPosition += filledQty
				bot.sellShortOrders = math.Max(0, bot.sellShortOrders-filledQty)
			}
		}

	case futures.OrderStatusTypeCanceled:
		if event.Side == futures.SideTypeBuy {
			if event.PositionSide == futures.PositionSideTypeLong {
				bot.buyLongOrders = math.Max(0, bot.buyLongOrders-quantity)
			} else {
				bot.buyShortOrders = math.Max(0, bot.buyShortOrders-quantity)
			}
		} else {
			if event.PositionSide == futures.PositionSideTypeLong {
				bot.sellLongOrders = math.Max(0, bot.sellLongOrders-quantity)
			} else {
				bot.sellShortOrders = math.Max(0, bot.sellShortOrders-quantity)
			}
		}
	}

	// 打印订单更新日志
	log.Printf("订单更新: %s %s %s %.4f/%.4f @ %s",
		event.Side,
		event.PositionSide,
		event.Status,
		filledQty,
		quantity,
		event.OriginalQty,
	)
}

// handleAccountUpdate 处理账户更新
func (bot *GridTradingBot) handleAccountUpdate(event futures.WsAccountUpdate) {
	bot.lock.Lock()
	defer bot.lock.Unlock()

	for _, position := range event.Positions {
		if position.Symbol == bot.symbol {
			amount, _ := strconv.ParseFloat(position.Amount, 64)
			if position.Side == futures.PositionSideTypeLong {
				bot.longPosition = math.Abs(amount)
			} else if position.Side == futures.PositionSideTypeShort {
				bot.shortPosition = math.Abs(amount)
			}
		}
	}
}

// handleBookTicker 处理行情数据
func (bot *GridTradingBot) handleBookTicker(event *futures.WsBookTickerEvent) {
	bot.lock.Lock()
	defer bot.lock.Unlock()

	// 限制更新频率
	currentTime := time.Now().Unix()
	if currentTime-bot.lastTickerUpdateTime < 1 { // 限制为1秒更新一次
		return
	}
	bot.lastTickerUpdateTime = currentTime

	// 更新价格
	bestBid, _ := strconv.ParseFloat(event.BestBidPrice, 64)
	bestAsk, _ := strconv.ParseFloat(event.BestAskPrice, 64)
	bot.bestBidPrice = bestBid
	bot.bestAskPrice = bestAsk
	bot.latestPrice = (bestBid + bestAsk) / 2

	// 检查持仓状态是否需要同步
	if currentTime-bot.lastPositionUpdateTime > SyncTime {
		if err := bot.syncPositions(); err != nil {
			log.Printf("同步持仓信息失败: %v", err)
		}
		bot.lastPositionUpdateTime = currentTime
		log.Printf("同步持仓信息: 多头=%.4f, 空头=%.4f", bot.longPosition, bot.shortPosition)
	}

	// 检查订单状态是否需要同步
	if currentTime-bot.lastOrdersUpdateTime > SyncTime {
		if err := bot.syncOrders(); err != nil {
			log.Printf("同步订单信息失败: %v", err)
		}
		bot.lastOrdersUpdateTime = currentTime
		log.Printf("同步订单信息: 多头买入=%.4f, 多头卖出=%.4f, 空头卖出=%.4f, 空头买入=%.4f",
			bot.buyLongOrders, bot.sellLongOrders, bot.sellShortOrders, bot.buyShortOrders)
	}

	// 执行网格策略
	if err := bot.adjustGridStrategy(); err != nil {
		log.Printf("调整网格策略失败: %v", err)
	}
}

// keepAliveListenKey 保持listenKey活跃
func (bot *GridTradingBot) keepAliveListenKey(listenKey string, stopC chan struct{}) {
	ticker := time.NewTicker(time.Minute * 30)
	defer ticker.Stop()

	for {
		select {
		case <-stopC:
			return
		case <-ticker.C:
			maxRetries := 3
			for retry := 0; retry < maxRetries; retry++ {
				err := bot.client.NewKeepaliveUserStreamService().ListenKey(listenKey).Do(context.Background())
				if err == nil {
					break
				}
				log.Printf("更新listenKey失败(重试 %d/%d): %v", retry+1, maxRetries, err)
				if retry < maxRetries-1 {
					time.Sleep(time.Second * 5)
				}
			}
		}
	}
}

// syncPositions 同步持仓信息
func (bot *GridTradingBot) syncPositions() error {
	positions, err := bot.client.NewGetPositionRiskService().Symbol(bot.symbol).Do(context.Background())
	if err != nil {
		return fmt.Errorf("获取持仓信息失败: %v", err)
	}

	for _, position := range positions {
		amount, _ := strconv.ParseFloat(position.PositionAmt, 64)
		if position.PositionSide == "LONG" {
			bot.longPosition = math.Abs(amount)
		} else if position.PositionSide == "SHORT" {
			bot.shortPosition = math.Abs(amount)
		}
	}

	log.Printf("同步持仓信息: 多头=%.4f, 空头=%.4f", bot.longPosition, bot.shortPosition)
	return nil
}

// syncOrders 同步订单信息
func (bot *GridTradingBot) syncOrders() error {
	orders, err := bot.client.NewListOpenOrdersService().Symbol(bot.symbol).Do(context.Background())
	if err != nil {
		return fmt.Errorf("获取订单信息失败: %v", err)
	}

	// 重置订单数量
	bot.buyLongOrders = 0
	bot.sellLongOrders = 0
	bot.sellShortOrders = 0
	bot.buyShortOrders = 0

	for _, order := range orders {
		quantity, _ := strconv.ParseFloat(order.OrigQuantity, 64)
		executedQty, _ := strconv.ParseFloat(order.ExecutedQuantity, 64)
		remaining := quantity - executedQty

		if order.Side == futures.SideTypeBuy {
			if order.PositionSide == futures.PositionSideTypeLong {
				bot.buyLongOrders += remaining
			} else {
				bot.buyShortOrders += remaining
			}
		} else {
			if order.PositionSide == futures.PositionSideTypeLong {
				bot.sellLongOrders += remaining
			} else {
				bot.sellShortOrders += remaining
			}
		}
	}

	log.Printf("同步订单信息: 多头买入=%.4f, 多头卖出=%.4f, 空头卖出=%.4f, 空头买入=%.4f",
		bot.buyLongOrders, bot.sellLongOrders, bot.sellShortOrders, bot.buyShortOrders)
	return nil
}

// adjustGridStrategy 调整网格策略
func (bot *GridTradingBot) adjustGridStrategy() error {
	// 检查并减少持仓
	if err := bot.checkAndReducePositions(); err != nil {
		return err
	}

	// 处理多头策略
	if bot.longPosition == 0 {
		if err := bot.initializeLongOrders(); err != nil {
			return fmt.Errorf("初始化多头订单失败: %v", err)
		}
	} else {
		ordersValid := !(0 < bot.buyLongOrders && bot.buyLongOrders <= bot.longInitialQty) ||
			!(0 < bot.sellLongOrders && bot.sellLongOrders <= bot.longInitialQty)

		if ordersValid {
			if bot.longPosition < PositionThreshold {
				// 如果持仓没到阈值，同步后再次确认
				if err := bot.syncOrders(); err != nil {
					return err
				}
				if ordersValid {
					if err := bot.placeLongOrders(); err != nil {
						return err
					}
				}
			} else {
				if err := bot.placeLongOrders(); err != nil {
					return err
				}
			}
		}
	}

	// 处理空头策略
	if bot.shortPosition == 0 {
		if err := bot.initializeShortOrders(); err != nil {
			return fmt.Errorf("初始化空头订单失败: %v", err)
		}
	} else {
		ordersValid := !(0 < bot.sellShortOrders && bot.sellShortOrders <= bot.shortInitialQty) ||
			!(0 < bot.buyShortOrders && bot.buyShortOrders <= bot.shortInitialQty)

		if ordersValid {
			if bot.shortPosition < PositionThreshold {
				// 如果持仓没到阈值，同步后再次确认
				if err := bot.syncOrders(); err != nil {
					return err
				}
				if ordersValid {
					if err := bot.placeShortOrders(); err != nil {
						return err
					}
				}
			} else {
				if err := bot.placeShortOrders(); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

// placeLongOrders 处理多头订单
func (bot *GridTradingBot) placeLongOrders() error {
	bot.getTakeProfitQuantity(bot.longPosition, futures.PositionSideTypeLong)

	if bot.longPosition > 0 {
		if bot.longPosition > PositionThreshold {
			log.Printf("持仓%.4f超过极限阈值%.4f，long装死", bot.longPosition, PositionThreshold)
			if bot.sellLongOrders <= 0 && bot.shortPosition > 0 {
				r := float64(bot.longPosition/bot.shortPosition)/100 + 1
				return bot.placeTakeProfitOrder(futures.PositionSideTypeLong, bot.latestPrice*r, bot.longInitialQty)
			}
		} else {
			bot.updateMidPrice(futures.PositionSideTypeLong, bot.latestPrice)
			if err := bot.cancelOrdersForSide(futures.PositionSideTypeLong); err != nil {
				return err
			}
			if err := bot.placeTakeProfitOrder(futures.PositionSideTypeLong, bot.upperPriceLong, bot.longInitialQty); err != nil {
				return err
			}
			if err := bot.placeOrder(futures.SideTypeBuy, futures.PositionSideTypeLong, bot.lowerPriceLong, bot.longInitialQty, false); err != nil {
				return err
			}
			log.Println("挂多头止盈，挂多头补仓")
		}
	}
	return nil
}

// placeShortOrders 处理空头订单
func (bot *GridTradingBot) placeShortOrders() error {
	bot.getTakeProfitQuantity(bot.shortPosition, futures.PositionSideTypeShort)

	if bot.shortPosition > 0 {
		if bot.shortPosition > PositionThreshold {
			log.Printf("持仓%.4f超过极限阈值%.4f，short装死", bot.shortPosition, PositionThreshold)
			if bot.buyShortOrders <= 0 && bot.longPosition > 0 {
				r := float64(bot.shortPosition/bot.longPosition)/100 + 1
				return bot.placeTakeProfitOrder(futures.PositionSideTypeShort, bot.latestPrice*r, bot.shortInitialQty)
			}
		} else {
			bot.updateMidPrice(futures.PositionSideTypeShort, bot.latestPrice)
			if err := bot.cancelOrdersForSide(futures.PositionSideTypeShort); err != nil {
				return err
			}
			if err := bot.placeTakeProfitOrder(futures.PositionSideTypeShort, bot.lowerPriceShort, bot.shortInitialQty); err != nil {
				return err
			}
			if err := bot.placeOrder(futures.SideTypeSell, futures.PositionSideTypeShort, bot.upperPriceShort, bot.shortInitialQty, false); err != nil {
				return err
			}
			log.Println("挂空头止盈，挂空头补仓")
		}
	}
	return nil
}
