package bbgo

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/codingconcepts/env"
	"github.com/jmoiron/sqlx"
	log "github.com/sirupsen/logrus"

	"github.com/c9s/bbgo/pkg/accounting/pnl"
	"github.com/c9s/bbgo/pkg/service"
	"github.com/c9s/bbgo/pkg/types"
	"github.com/c9s/bbgo/pkg/util"
)

var LoadedExchangeStrategies = make(map[string]SingleExchangeStrategy)
var LoadedCrossExchangeStrategies = make(map[string]CrossExchangeStrategy)

func RegisterStrategy(key string, s interface{}) {
	switch d := s.(type) {
	case SingleExchangeStrategy:
		LoadedExchangeStrategies[key] = d

	case CrossExchangeStrategy:
		LoadedCrossExchangeStrategies[key] = d

	default:
		panic(fmt.Errorf("%T does not implement SingleExchangeStrategy or CrossExchangeStrategy", d))
	}
}

var emptyTime time.Time

// Environment presents the real exchange data layer
type Environment struct {
	// Notifiability here for environment is for the streaming data notification
	// note that, for back tests, we don't need notification.
	Notifiability

	PersistenceServiceFacade *PersistenceServiceFacade

	TradeService *service.TradeService
	TradeSync    *service.SyncService

	// startTime is the time of start point (which is used in the backtest)
	startTime     time.Time
	tradeScanTime time.Time
	sessions      map[string]*ExchangeSession
}

func NewEnvironment() *Environment {
	return &Environment{
		// default trade scan time
		tradeScanTime: time.Now().AddDate(0, 0, -7), // sync from 7 days ago
		sessions:      make(map[string]*ExchangeSession),
	}
}

func (environ *Environment) Sessions() map[string]*ExchangeSession {
	return environ.sessions
}

func (environ *Environment) SyncTrades(db *sqlx.DB) *Environment {
	environ.TradeService = &service.TradeService{DB: db}
	environ.TradeSync = &service.SyncService{
		TradeService: environ.TradeService,
	}

	return environ
}

func (environ *Environment) AddExchange(name string, exchange types.Exchange) (session *ExchangeSession) {
	session = NewExchangeSession(name, exchange)
	environ.sessions[name] = session
	return session
}

// Init prepares the data that will be used by the strategies
func (environ *Environment) Init(ctx context.Context) (err error) {
	for n := range environ.sessions {
		var session = environ.sessions[n]
		var markets, err = LoadExchangeMarketsWithCache(ctx, session.Exchange)

		if len(markets) == 0 {
			return fmt.Errorf("market config should not be empty")
		}

		session.markets = markets

		// trade sync and market data store depends on subscribed symbols so we have to do this here.
		for symbol := range session.loadedSymbols {
			var trades []types.Trade

			if environ.TradeSync != nil {
				log.Infof("syncing trades from %s for symbol %s...", session.Exchange.Name(), symbol)
				if err := environ.TradeSync.SyncTrades(ctx, session.Exchange, symbol, environ.tradeScanTime); err != nil {
					return err
				}

				tradingFeeCurrency := session.Exchange.PlatformFeeCurrency()
				if strings.HasPrefix(symbol, tradingFeeCurrency) {
					trades, err = environ.TradeService.QueryForTradingFeeCurrency(session.Exchange.Name(), symbol, tradingFeeCurrency)
				} else {
					trades, err = environ.TradeService.Query(session.Exchange.Name(), symbol)
				}

				if err != nil {
					return err
				}

				log.Infof("symbol %s: %d trades loaded", symbol, len(trades))
			}

			session.Trades[symbol] = trades
			session.lastPrices[symbol] = 0.0

			marketDataStore := NewMarketDataStore(symbol)
			marketDataStore.BindStream(session.Stream)
			session.marketDataStores[symbol] = marketDataStore

			standardIndicatorSet := NewStandardIndicatorSet(symbol, marketDataStore)
			session.standardIndicatorSets[symbol] = standardIndicatorSet
		}

		log.Infof("querying balances from session %s...", session.Name)
		balances, err := session.Exchange.QueryAccountBalances(ctx)
		if err != nil {
			return err
		}

		log.Infof("%s account", session.Name)
		balances.Print()

		session.Account.UpdateBalances(balances)
		session.Account.BindStream(session.Stream)

		session.Stream.OnBalanceUpdate(func(balances types.BalanceMap) {
			log.Infof("balance update: %+v", balances)
		})

		// update last prices
		session.Stream.OnKLineClosed(func(kline types.KLine) {
			log.Infof("kline closed: %+v", kline)

			if _, ok := session.startPrices[kline.Symbol]; !ok {
				session.startPrices[kline.Symbol] = kline.Open
			}

			session.lastPrices[kline.Symbol] = kline.Close
		})

		session.Stream.OnTradeUpdate(func(trade types.Trade) {
			session.Trades[trade.Symbol] = append(session.Trades[trade.Symbol], trade)
		})

		// feed klines into the market data store
		if environ.startTime == emptyTime {
			environ.startTime = time.Now()
		}

		var intervals = map[types.Interval]struct{}{}
		for _, sub := range session.Subscriptions {
			if sub.Channel == types.KLineChannel {
				intervals[types.Interval(sub.Options.Interval)] = struct{}{}
			}
		}

		for symbol := range session.loadedSymbols {
			marketDataStore, ok := session.marketDataStores[symbol]
			if !ok {
				return fmt.Errorf("symbol %s is not defined", symbol)
			}

			var lastPriceTime time.Time
			for interval := range intervals {
				// avoid querying the last unclosed kline
				endTime := environ.startTime.Add(- interval.Duration())
				kLines, err := session.Exchange.QueryKLines(ctx, symbol, interval, types.KLineQueryOptions{
					EndTime: &endTime,
					Limit:   1000, // indicators need at least 100
				})
				if err != nil {
					return err
				}

				if len(kLines) == 0 {
					log.Warnf("no kline data for interval %s (end time <= %s)", interval, environ.startTime)
					continue
				}

				// update last prices by the given kline
				lastKLine := kLines[len(kLines)-1]
				log.Infof("last kline: %+v", lastKLine)
				if lastPriceTime == emptyTime {
					session.lastPrices[symbol] = lastKLine.Close
					lastPriceTime = lastKLine.EndTime
				} else if lastPriceTime.Before(lastKLine.EndTime) {
					session.lastPrices[symbol] = lastKLine.Close
					lastPriceTime = lastKLine.EndTime
				}

				for _, k := range kLines {
					// let market data store trigger the update, so that the indicator could be updated too.
					marketDataStore.AddKLine(k)
				}
			}
		}

		if environ.TradeService != nil {
			session.Stream.OnTradeUpdate(func(trade types.Trade) {
				if err := environ.TradeService.Insert(trade); err != nil {
					log.WithError(err).Errorf("trade insert error: %+v", trade)
				}
			})
		}

		// TODO: move market data store dispatch to here, use one callback to dispatch the market data
		// Session.Stream.OnKLineClosed(func(kline types.KLine) { })
	}

	return nil
}

func (environ *Environment) ConfigurePersistence(conf *PersistenceConfig) error {
	var facade = &PersistenceServiceFacade{
		Memory: NewMemoryService(),
	}

	if conf.Redis != nil {
		if err := env.Set(conf.Redis); err != nil {
			return err
		}

		facade.Redis = NewRedisPersistenceService(conf.Redis)
	}

	if conf.Json != nil {
		if _, err := os.Stat(conf.Json.Directory); os.IsNotExist(err) {
			if err2 := os.MkdirAll(conf.Json.Directory, 0777); err2 != nil {
				log.WithError(err2).Errorf("can not create directory: %s", conf.Json.Directory)
				return err2
			}
		}

		facade.Json = &JsonPersistenceService{Directory: conf.Json.Directory}
	}

	environ.PersistenceServiceFacade = facade
	return nil
}

// configure notification rules
// for symbol-based routes, we should register the same symbol rules for each session.
// for session-based routes, we should set the fixed callbacks for each session
func (environ *Environment) ConfigureNotification(conf *NotificationConfig) error {
	// configure routing here
	if conf.SymbolChannels != nil {
		environ.SymbolChannelRouter.AddRoute(conf.SymbolChannels)
	}
	if conf.SessionChannels != nil {
		environ.SessionChannelRouter.AddRoute(conf.SessionChannels)
	}

	if conf.Routing != nil {
		// configure passive object notification routing
		switch conf.Routing.Trade {
		case "$silent": // silent, do not setup notification

		case "$session":
			defaultTradeUpdateHandler := func(trade types.Trade) {
				text := util.Render(TemplateTradeReport, trade)
				environ.Notify(text, &trade)
			}
			for name := range environ.sessions {
				session := environ.sessions[name]

				// if we can route session name to channel successfully...
				channel, ok := environ.SessionChannelRouter.Route(name)
				if ok {
					session.Stream.OnTradeUpdate(func(trade types.Trade) {
						text := util.Render(TemplateTradeReport, trade)
						environ.NotifyTo(channel, text, &trade)
					})
				} else {
					session.Stream.OnTradeUpdate(defaultTradeUpdateHandler)
				}
			}

		case "$symbol":
			// configure object routes for Trade
			environ.ObjectChannelRouter.Route(func(obj interface{}) (channel string, ok bool) {
				trade, matched := obj.(*types.Trade)
				if !matched {
					return
				}
				channel, ok = environ.SymbolChannelRouter.Route(trade.Symbol)
				return
			})

			// use same handler for each session
			handler := func(trade types.Trade) {
				text := util.Render(TemplateTradeReport, trade)
				channel, ok := environ.RouteObject(&trade)
				if ok {
					environ.NotifyTo(channel, text, &trade)
				} else {
					environ.Notify(text, &trade)
				}
			}
			for _, session := range environ.sessions {
				session.Stream.OnTradeUpdate(handler)
			}
		}

		switch conf.Routing.Order {

		case "$silent": // silent, do not setup notification

		case "$session":
			defaultOrderUpdateHandler := func(order types.Order) {
				text := util.Render(TemplateOrderReport, order)
				environ.Notify(text, &order)
			}
			for name := range environ.sessions {
				session := environ.sessions[name]

				// if we can route session name to channel successfully...
				channel, ok := environ.SessionChannelRouter.Route(name)
				if ok {
					session.Stream.OnOrderUpdate(func(order types.Order) {
						text := util.Render(TemplateOrderReport, order)
						environ.NotifyTo(channel, text, &order)
					})
				} else {
					session.Stream.OnOrderUpdate(defaultOrderUpdateHandler)
				}
			}

		case "$symbol":
			// add object route
			environ.ObjectChannelRouter.Route(func(obj interface{}) (channel string, ok bool) {
				order, matched := obj.(*types.Order)
				if !matched {
					return
				}
				channel, ok = environ.SymbolChannelRouter.Route(order.Symbol)
				return
			})

			// use same handler for each session
			handler := func(order types.Order) {
				text := util.Render(TemplateOrderReport, order)
				channel, ok := environ.RouteObject(&order)
				if ok {
					environ.NotifyTo(channel, text, &order)
				} else {
					environ.Notify(text, &order)
				}
			}
			for _, session := range environ.sessions {
				session.Stream.OnOrderUpdate(handler)
			}
		}

		switch conf.Routing.SubmitOrder {

		case "$silent": // silent, do not setup notification

		case "$symbol":
			// add object route
			environ.ObjectChannelRouter.Route(func(obj interface{}) (channel string, ok bool) {
				order, matched := obj.(*types.SubmitOrder)
				if !matched {
					return
				}

				channel, ok = environ.SymbolChannelRouter.Route(order.Symbol)
				return
			})

		}

		// currently not used
		switch conf.Routing.PnL {
		case "$symbol":
			environ.ObjectChannelRouter.Route(func(obj interface{}) (channel string, ok bool) {
				report, matched := obj.(*pnl.AverageCostPnlReport)
				if !matched {
					return
				}
				channel, ok = environ.SymbolChannelRouter.Route(report.Symbol)
				return
			})
		}

	}
	return nil
}

func (environ *Environment) SetStartTime(t time.Time) *Environment {
	environ.startTime = t
	return environ
}

// SyncTradesFrom overrides the default trade scan time (-7 days)
func (environ *Environment) SyncTradesFrom(t time.Time) *Environment {
	environ.tradeScanTime = t
	return environ
}

func (environ *Environment) Connect(ctx context.Context) error {
	for n := range environ.sessions {
		// avoid using the placeholder variable for the session because we use that in the callbacks
		var session = environ.sessions[n]
		var logger = log.WithField("session", n)

		if len(session.Subscriptions) == 0 {
			logger.Warnf("exchange session %s has no subscriptions", session.Name)
		} else {
			// add the subscribe requests to the stream
			for _, s := range session.Subscriptions {
				logger.Infof("subscribing %s %s %v", s.Symbol, s.Channel, s.Options)
				session.Stream.Subscribe(s.Channel, s.Symbol, s.Options)
			}
		}

		logger.Infof("connecting session %s...", session.Name)
		if err := session.Stream.Connect(ctx); err != nil {
			return err
		}
	}

	return nil
}

func LoadExchangeMarketsWithCache(ctx context.Context, ex types.Exchange) (markets types.MarketMap, err error) {
	err = WithCache(fmt.Sprintf("%s-markets", ex.Name()), &markets, func() (interface{}, error) {
		return ex.QueryMarkets(ctx)
	})
	return markets, err
}
