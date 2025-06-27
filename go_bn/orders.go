package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"time"

	"github.com/adshao/go-binance/v2/futures"
)

// placeOrder 下单函数
func (bot *GridTradingBot) placeOrder(side futures.SideType, positionSide futures.PositionSideType, price float64, quantity float64, reduceOnly bool) error {
	// 调整价格和数量精度
	price = math.Floor(price*math.Pow10(bot.pricePrecision)) / math.Pow10(bot.pricePrecision)
	quantity = math.Floor(quantity*math.Pow10(bot.quantityPrecision)) / math.Pow10(bot.quantityPrecision)

	// 确保数量不小于最小下单量
	if quantity < bot.minOrderQuantity {
		quantity = bot.minOrderQuantity
	}

	// 创建订单
	order := bot.client.NewCreateOrderService().
		Symbol(bot.symbol).
		Side(side).
		PositionSide(positionSide).
		Type(futures.OrderTypeLimit).
		TimeInForce(futures.TimeInForceTypeGTC).
		Quantity(fmt.Sprintf("%.*f", bot.quantityPrecision, quantity)).
		Price(fmt.Sprintf("%.*f", bot.pricePrecision, price)).
		ReduceOnly(reduceOnly)

	_, err := order.Do(context.Background())
	if err != nil {
		return fmt.Errorf("下单失败: %v", err)
	}

	log.Printf("下单成功: %s %s %.4f @ %.4f", side, positionSide, quantity, price)
	return nil
}

// placeTakeProfitOrder 下止盈单
func (bot *GridTradingBot) placeTakeProfitOrder(positionSide futures.PositionSideType, price float64, quantity float64) error {
	var side futures.SideType
	if positionSide == futures.PositionSideTypeLong {
		side = futures.SideTypeSell
	} else {
		side = futures.SideTypeBuy
	}

	return bot.placeOrder(side, positionSide, price, quantity, true)
}

// cancelOrdersForSide 撤销某个方向的所有挂单
func (bot *GridTradingBot) cancelOrdersForSide(positionSide futures.PositionSideType) error {
	orders, err := bot.client.NewListOpenOrdersService().Symbol(bot.symbol).Do(context.Background())
	if err != nil {
		return fmt.Errorf("获取挂单失败: %v", err)
	}

	for _, order := range orders {
		if order.PositionSide == positionSide {
			_, err := bot.client.NewCancelOrderService().
				Symbol(bot.symbol).
				OrderID(order.OrderID).
				Do(context.Background())

			if err != nil {
				log.Printf("撤销订单 %d 失败: %v", order.OrderID, err)
				continue
			}
			log.Printf("撤销订单成功: %d", order.OrderID)
		}
	}
	return nil
}

// updateMidPrice 更新中间价格
func (bot *GridTradingBot) updateMidPrice(positionSide futures.PositionSideType, price float64) {
	if positionSide == futures.PositionSideTypeLong {
		bot.midPriceLong = price
		bot.upperPriceLong = price * (1 + GridSpacing)
		bot.lowerPriceLong = price * (1 - GridSpacing)
		log.Println("更新多头中间价格")
	} else {
		bot.midPriceShort = price
		bot.upperPriceShort = price * (1 + GridSpacing)
		bot.lowerPriceShort = price * (1 - GridSpacing)
		log.Println("更新空头中间价格")
	}
}

// initializeLongOrders 初始化多头订单
func (bot *GridTradingBot) initializeLongOrders() error {
	bot.lock.Lock()
	defer bot.lock.Unlock()

	// 检查时间间隔
	if time.Now().Unix()-bot.lastLongOrderTime < OrderFirstTime {
		log.Printf("距离上次多头挂单时间不足 %d 秒，跳过本次挂单", OrderFirstTime)
		return nil
	}

	// 撤销现有多头订单
	if err := bot.cancelOrdersForSide(futures.PositionSideTypeLong); err != nil {
		return err
	}

	// 下新的多头开仓单
	if err := bot.placeOrder(futures.SideTypeBuy, futures.PositionSideTypeLong, bot.bestBidPrice, bot.longInitialQty, false); err != nil {
		return err
	}

	bot.lastLongOrderTime = time.Now().Unix()
	log.Println("初始化多头挂单完成")
	return nil
}

// initializeShortOrders 初始化空头订单
func (bot *GridTradingBot) initializeShortOrders() error {
	bot.lock.Lock()
	defer bot.lock.Unlock()

	// 检查时间间隔
	if time.Now().Unix()-bot.lastShortOrderTime < OrderFirstTime {
		log.Printf("距离上次空头挂单时间不足 %d 秒，跳过本次挂单", OrderFirstTime)
		return nil
	}

	// 撤销现有空头订单
	if err := bot.cancelOrdersForSide(futures.PositionSideTypeShort); err != nil {
		return err
	}

	// 下新的空头开仓单
	if err := bot.placeOrder(futures.SideTypeSell, futures.PositionSideTypeShort, bot.bestAskPrice, bot.shortInitialQty, false); err != nil {
		return err
	}

	bot.lastShortOrderTime = time.Now().Unix()
	log.Println("初始化空头挂单完成")
	return nil
}

// checkAndReducePositions 检查并减少持仓
func (bot *GridTradingBot) checkAndReducePositions() error {
	localPositionThreshold := PositionThreshold * 0.8 // 阈值的80%
	quantity := PositionThreshold * 0.1               // 阈值的10%

	if bot.longPosition >= localPositionThreshold && bot.shortPosition >= localPositionThreshold {
		log.Printf("多头和空头持仓均超过阈值 %.2f，开始双向平仓，减少库存风险", localPositionThreshold)

		// 平多头仓位
		if bot.longPosition > 0 {
			if err := bot.placeOrder(futures.SideTypeSell, futures.PositionSideTypeLong, bot.bestAskPrice, quantity, true); err != nil {
				return fmt.Errorf("平多头失败: %v", err)
			}
		}

		// 平空头仓位
		if bot.shortPosition > 0 {
			if err := bot.placeOrder(futures.SideTypeBuy, futures.PositionSideTypeShort, bot.bestBidPrice, quantity, true); err != nil {
				return fmt.Errorf("平空头失败: %v", err)
			}
		}
	}
	return nil
}

// getTakeProfitQuantity 获取止盈数量
func (bot *GridTradingBot) getTakeProfitQuantity(position float64, positionSide futures.PositionSideType) float64 {
	if positionSide == futures.PositionSideTypeLong {
		if position > PositionLimit {
			bot.longInitialQty = InitialQuantity * 2
		} else if bot.shortPosition >= PositionThreshold {
			bot.longInitialQty = InitialQuantity * 2
		} else {
			bot.longInitialQty = InitialQuantity
		}
		return bot.longInitialQty
	} else {
		if position > PositionLimit {
			bot.shortInitialQty = InitialQuantity * 2
		} else if bot.longPosition >= PositionThreshold {
			bot.shortInitialQty = InitialQuantity * 2
		} else {
			bot.shortInitialQty = InitialQuantity
		}
		return bot.shortInitialQty
	}
}

// monitorOrders 监控订单状态
func (bot *GridTradingBot) monitorOrders() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for range ticker.C {
		orders, err := bot.client.NewListOpenOrdersService().Symbol(bot.symbol).Do(context.Background())
		if err != nil {
			log.Printf("获取订单失败: %v", err)
			continue
		}

		for _, order := range orders {
			// 获取订单时间
			orderTime := order.Time
			// 如果订单超过5分钟未成交，则取消
			if time.Since(time.Unix(0, orderTime*int64(time.Millisecond))) > 5*time.Minute {
				log.Printf("订单 %d 超过5分钟未成交，准备取消", order.OrderID)
				if err := bot.cancelOrder(order.OrderID); err != nil {
					log.Printf("取消订单 %d 失败: %v", order.OrderID, err)
				} else {
					log.Printf("已取消超时订单 %d", order.OrderID)
				}
			}
		}
	}
}

// cancelOrder 取消指定订单
func (bot *GridTradingBot) cancelOrder(orderID int64) error {
	_, err := bot.client.NewCancelOrderService().
		Symbol(bot.symbol).
		OrderID(orderID).
		Do(context.Background())

	if err != nil {
		return fmt.Errorf("取消订单失败: %v", err)
	}
	return nil
}
