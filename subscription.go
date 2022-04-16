package centrifuge

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/centrifugal/protocol"
)

// SubState represents state of Subscription.
type SubState string

// Different states of Subscription.
const (
	SubStateUnsubscribed SubState = "unsubscribed"
	SubStateSubscribing  SubState = "subscribing"
	SubStateSubscribed   SubState = "subscribed"
)

// SubscriptionConfig allows setting Subscription options.
type SubscriptionConfig struct {
	// Data is an arbitrary data to pass to a server in each subscribe request.
	Data []byte
	// Token for Subscription.
	Token string
}

func newSubscription(c *Client, channel string, config ...SubscriptionConfig) *Subscription {
	s := &Subscription{
		Channel:             channel,
		centrifuge:          c,
		state:               SubStateUnsubscribed,
		events:              newSubscriptionEventHub(),
		subFutures:          make(map[uint64]subFuture),
		resubscribeStrategy: defaultBackoffReconnect,
	}
	if len(config) == 1 {
		cfg := config[0]
		s.token = cfg.Token
		s.data = cfg.Data
	}
	return s
}

// Subscription represents client subscription to channel.
type Subscription struct {
	futureID   uint64
	mu         sync.RWMutex
	centrifuge *Client

	// Channel for a subscription.
	Channel string

	state SubState

	events     *subscriptionEventHub
	offset     uint64
	epoch      string
	recover    bool
	err        error
	subFutures map[uint64]subFuture
	data       []byte

	token string

	resubscribeAttempts int
	resubscribeStrategy reconnectStrategy

	resubscribeTimer *time.Timer
	refreshTimer     *time.Timer
}

func (s *Subscription) State() SubState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.state
}

type subFuture struct {
	fn      func(error)
	closeCh chan struct{}
}

func newSubFuture(fn func(error)) subFuture {
	return subFuture{fn: fn, closeCh: make(chan struct{})}
}

func (s *Subscription) nextFutureID() uint64 {
	return atomic.AddUint64(&s.futureID, 1)
}

// Lock must be held outside.
func (s *Subscription) resolveSubFutures(err error) {
	for _, fut := range s.subFutures {
		fut.fn(err)
		close(fut.closeCh)
	}
	s.subFutures = make(map[uint64]subFuture)
}

// Publish allows publishing data to the subscription channel.
func (s *Subscription) Publish(data []byte) (PublishResult, error) {
	s.mu.Lock()
	if s.state == SubStateUnsubscribed {
		s.mu.Unlock()
		return PublishResult{}, ErrSubscriptionUnsubscribed
	}
	s.mu.Unlock()

	resCh := make(chan PublishResult, 1)
	errCh := make(chan error, 1)
	s.publish(data, func(result PublishResult, err error) {
		resCh <- result
		errCh <- err
	})
	return <-resCh, <-errCh
}

type HistoryOptions struct {
	Limit   int32
	Since   *StreamPosition
	Reverse bool
}

type HistoryOption func(options *HistoryOptions)

func WithHistorySince(sp *StreamPosition) HistoryOption {
	return func(options *HistoryOptions) {
		options.Since = sp
	}
}

func WithHistoryLimit(limit int32) HistoryOption {
	return func(options *HistoryOptions) {
		options.Limit = limit
	}
}

func WithHistoryReverse(reverse bool) HistoryOption {
	return func(options *HistoryOptions) {
		options.Reverse = reverse
	}
}

// History allows extracting channel history. By default, it returns current stream top
// position without publications. Use WithHistoryLimit with a value > 0 to make this func
// to return publications.
func (s *Subscription) History(opts ...HistoryOption) (HistoryResult, error) {
	historyOpts := &HistoryOptions{}
	for _, opt := range opts {
		opt(historyOpts)
	}
	s.mu.Lock()
	if s.state == SubStateUnsubscribed {
		s.mu.Unlock()
		return HistoryResult{}, ErrSubscriptionUnsubscribed
	}
	s.mu.Unlock()

	resCh := make(chan HistoryResult, 1)
	errCh := make(chan error, 1)
	s.history(*historyOpts, func(result HistoryResult, err error) {
		resCh <- result
		errCh <- err
	})
	return <-resCh, <-errCh
}

// Presence allows extracting channel presence.
func (s *Subscription) Presence() (PresenceResult, error) {
	s.mu.Lock()
	if s.state == SubStateUnsubscribed {
		s.mu.Unlock()
		return PresenceResult{}, ErrSubscriptionUnsubscribed
	}
	s.mu.Unlock()

	resCh := make(chan PresenceResult, 1)
	errCh := make(chan error, 1)
	s.presence(func(result PresenceResult, err error) {
		resCh <- result
		errCh <- err
	})
	return <-resCh, <-errCh
}

// PresenceStats allows extracting channel presence stats.
func (s *Subscription) PresenceStats() (PresenceStatsResult, error) {
	s.mu.Lock()
	if s.state == SubStateUnsubscribed {
		s.mu.Unlock()
		return PresenceStatsResult{}, ErrSubscriptionUnsubscribed
	}
	s.mu.Unlock()

	resCh := make(chan PresenceStatsResult, 1)
	errCh := make(chan error, 1)
	s.presenceStats(func(result PresenceStatsResult, err error) {
		resCh <- result
		errCh <- err
	})
	return <-resCh, <-errCh
}

func (s *Subscription) onSubscribe(fn func(err error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == SubStateSubscribed {
		go fn(nil)
	} else if s.state == SubStateUnsubscribed {
		go fn(ErrSubscriptionUnsubscribed)
	} else {
		id := s.nextFutureID()
		fut := newSubFuture(fn)
		s.subFutures[id] = fut
		go func() {
			select {
			case <-fut.closeCh:
			case <-time.After(s.centrifuge.config.ReadTimeout):
				s.mu.Lock()
				defer s.mu.Unlock()
				fut, ok := s.subFutures[id]
				if !ok {
					return
				}
				delete(s.subFutures, id)
				fut.fn(ErrTimeout)
			}
		}()
	}
}

func (s *Subscription) publish(data []byte, fn func(PublishResult, error)) {
	s.onSubscribe(func(err error) {
		if err != nil {
			fn(PublishResult{}, err)
			return
		}
		s.centrifuge.publish(s.Channel, data, fn)
	})
}

func (s *Subscription) history(opts HistoryOptions, fn func(HistoryResult, error)) {
	s.onSubscribe(func(err error) {
		if err != nil {
			fn(HistoryResult{}, err)
			return
		}
		s.centrifuge.history(s.Channel, opts, fn)
	})
}

func (s *Subscription) presence(fn func(PresenceResult, error)) {
	s.onSubscribe(func(err error) {
		if err != nil {
			fn(PresenceResult{}, err)
			return
		}
		s.centrifuge.presence(s.Channel, fn)
	})
}

func (s *Subscription) presenceStats(fn func(PresenceStatsResult, error)) {
	s.onSubscribe(func(err error) {
		if err != nil {
			fn(PresenceStatsResult{}, err)
			return
		}
		s.centrifuge.presenceStats(s.Channel, fn)
	})
}

// Unsubscribe allows unsubscribing from channel.
func (s *Subscription) Unsubscribe() error {
	if s.centrifuge.isClosed() {
		return ErrClientClosed
	}
	s.unsubscribe(unsubscribedUnsubscribeCalled, "unsubscribe called", true)
	return nil
}

// Lock must be held outside.
func (s *Subscription) unsubscribe(code uint32, reason string, sendUnsubscribe bool) {
	s.moveToUnsubscribed(code, reason)
	if sendUnsubscribe {
		s.centrifuge.unsubscribe(s.Channel, func(result UnsubscribeResult, err error) {
			if err != nil {
				go s.centrifuge.handleDisconnect(&disconnect{Code: connectingUnsubscribeError, Reason: "unsubscribe error", Reconnect: true})
				return
			}
		})
	}
}

// Subscribe allows initiating subscription process.
func (s *Subscription) Subscribe() error {
	if s.centrifuge.isClosed() {
		return ErrClientClosed
	}
	s.mu.Lock()
	if s.state == SubStateSubscribed || s.state == SubStateSubscribing {
		s.mu.Unlock()
		return nil
	}
	s.state = SubStateSubscribing
	s.mu.Unlock()

	if s.events != nil && s.events.onSubscribing != nil {
		handler := s.events.onSubscribing
		s.centrifuge.runHandlerAsync(func() {
			handler(SubscribingEvent{
				Code:   subscribingSubscribeCalled,
				Reason: "subscribe called",
			})
		})
	}

	s.resubscribe()
	return nil
}

func (s *Subscription) moveToUnsubscribed(code uint32, reason string) {
	s.mu.Lock()
	s.resubscribeAttempts = 0
	if s.resubscribeTimer != nil {
		s.resubscribeTimer.Stop()
	}
	if s.refreshTimer != nil {
		s.refreshTimer.Stop()
	}

	needEvent := s.state != SubStateUnsubscribed
	s.state = SubStateUnsubscribed
	s.mu.Unlock()

	if needEvent && s.events != nil && s.events.onUnsubscribe != nil {
		handler := s.events.onUnsubscribe
		s.centrifuge.runHandlerAsync(func() {
			handler(UnsubscribedEvent{
				Code:   code,
				Reason: reason,
			})
		})
	}
}

func (s *Subscription) moveToSubscribing(code uint32, reason string) {
	s.mu.Lock()
	s.resubscribeAttempts = 0
	if s.resubscribeTimer != nil {
		s.resubscribeTimer.Stop()
	}
	if s.refreshTimer != nil {
		s.refreshTimer.Stop()
	}
	needEvent := s.state != SubStateSubscribing
	s.state = SubStateSubscribing
	s.mu.Unlock()

	if needEvent && s.events != nil && s.events.onSubscribing != nil {
		handler := s.events.onSubscribing
		s.centrifuge.runHandlerAsync(func() {
			handler(SubscribingEvent{
				Code:   code,
				Reason: reason,
			})
		})
	}
}

func (s *Subscription) moveToSubscribed(res *protocol.SubscribeResult) {
	s.mu.Lock()
	if s.state != SubStateSubscribing {
		s.mu.Unlock()
		return
	}
	s.state = SubStateSubscribed
	if res.Expires {
		s.scheduleSubRefresh(res.Ttl)
	}
	if res.Recoverable {
		s.recover = true
	}
	s.resubscribeAttempts = 0
	if s.resubscribeTimer != nil {
		s.resubscribeTimer.Stop()
	}
	s.resolveSubFutures(nil)
	s.epoch = res.Epoch
	s.mu.Unlock()

	if s.events != nil && s.events.onSubscribed != nil {
		handler := s.events.onSubscribed
		ev := SubscribedEvent{Data: res.GetData(), Recovered: res.GetRecovered(), WasRecovering: res.GetWasRecovering()}
		s.centrifuge.runHandlerAsync(func() {
			handler(ev)
		})
	}

	if len(res.Publications) > 0 {
		s.centrifuge.runHandlerSync(func() {
			pubs := res.Publications
			for i := 0; i < len(pubs); i++ {
				pub := res.Publications[i]
				s.mu.Lock()
				if s.state != SubStateSubscribed {
					s.mu.Unlock()
					return
				}
				if pub.Offset > 0 {
					s.offset = pub.Offset
				}
				s.mu.Unlock()
				var handler PublicationHandler
				if s.events != nil && s.events.onPublication != nil {
					handler = s.events.onPublication
				}
				if handler != nil {
					s.centrifuge.runHandlerSync(func() {
						handler(PublicationEvent{Publication: pubFromProto(pub)})
					})
				}
			}
		})
	} else {
		s.mu.Lock()
		s.offset = res.Offset
		s.mu.Unlock()
	}
}

// Lock must be held outside.
func (s *Subscription) clearPositionState() {
	s.recover = false
	s.offset = 0
	s.epoch = ""
}

// Lock must be held outside.
func (s *Subscription) scheduleResubscribe() {
	delay := s.resubscribeStrategy.timeBeforeNextAttempt(s.resubscribeAttempts)
	s.resubscribeAttempts++
	s.resubscribeTimer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		if s.state != SubStateSubscribing {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()
		s.resubscribe()
	})
}

func (s *Subscription) subscribeError(err error) {
	s.mu.Lock()
	if s.state != SubStateSubscribing {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	if err == ErrTimeout {
		go s.centrifuge.handleDisconnect(&disconnect{Code: connectingSubscribeTimeout, Reason: "subscribe timeout", Reconnect: true})
		return
	}

	s.emitError(SubscriptionSubscribeError{Err: err})

	s.mu.Lock()
	defer s.mu.Unlock()
	var serverError *Error
	if errors.As(err, &serverError) {
		if serverError.Code == 109 { // Token expired.
			s.token = ""
			s.scheduleResubscribe()
		} else if serverError.Temporary {
			s.scheduleResubscribe()
		} else {
			s.resolveSubFutures(err)
			s.unsubscribe(serverError.Code, serverError.Message, false)
		}
	} else {
		s.scheduleResubscribe()
	}
}

// Lock must be held outside.
func (s *Subscription) emitError(err error) {
	if s.events != nil && s.events.onError != nil {
		handler := s.events.onError
		s.centrifuge.runHandlerSync(func() {
			handler(SubscriptionErrorEvent{Error: err})
		})
	}
}

func (s *Subscription) handlePublication(pub *protocol.Publication) {
	s.mu.Lock()
	if s.state != SubStateSubscribed {
		s.mu.Unlock()
		return
	}
	if pub.Offset > 0 {
		s.offset = pub.Offset
	}
	s.mu.Unlock()

	var handler PublicationHandler
	if s.events != nil && s.events.onPublication != nil {
		handler = s.events.onPublication
	}
	if handler == nil {
		return
	}
	s.centrifuge.runHandlerSync(func() {
		handler(PublicationEvent{Publication: pubFromProto(pub)})
	})
}

func (s *Subscription) handleJoin(info *protocol.ClientInfo) {
	var handler JoinHandler
	if s.events != nil && s.events.onJoin != nil {
		handler = s.events.onJoin
	}
	if handler != nil {
		s.centrifuge.runHandlerSync(func() {
			handler(JoinEvent{ClientInfo: infoFromProto(info)})
		})
	}
}

func (s *Subscription) handleLeave(info *protocol.ClientInfo) {
	var handler LeaveHandler
	if s.events != nil && s.events.onLeave != nil {
		handler = s.events.onLeave
	}
	if handler != nil {
		s.centrifuge.runHandlerSync(func() {
			handler(LeaveEvent{ClientInfo: infoFromProto(info)})
		})
	}
}

func (s *Subscription) handleUnsubscribe(unsubscribe *protocol.Unsubscribe) {
	if unsubscribe.Code < 2500 {
		s.moveToSubscribing(unsubscribe.Code, "server")
		s.resubscribe()
	} else {
		s.moveToUnsubscribed(unsubscribe.Code, "server")
	}
}

func (s *Subscription) resubscribe() {
	if s.centrifuge.state != StateConnected {
		return
	}
	s.mu.Lock()
	if s.state != SubStateSubscribing {
		s.mu.Unlock()
		return
	}
	token := s.token
	s.mu.Unlock()

	if strings.HasPrefix(s.Channel, s.centrifuge.config.PrivateChannelPrefix) && token == "" {
		var err error
		token, err = s.getSubscriptionToken(s.Channel)
		if err != nil {
			s.subscribeError(err)
			return
		}
		s.mu.Lock()
		if token == "" {
			s.unsubscribe(unsubscribedUnauthorized, "unauthorized", true)
			s.mu.Unlock()
			return
		}
		s.token = token
		s.mu.Unlock()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != SubStateSubscribing {
		return
	}

	var isRecover bool
	var sp StreamPosition
	if s.recover {
		isRecover = true
		sp.Offset = s.offset
		sp.Epoch = s.epoch
	}

	err := s.centrifuge.sendSubscribe(s.Channel, s.data, isRecover, sp, token, func(res *protocol.SubscribeResult, err error) {
		if err != nil {
			s.subscribeError(err)
			return
		}
		s.moveToSubscribed(res)
	})
	if err != nil {
		s.scheduleResubscribe()
	}
}

func (s *Subscription) getSubscriptionToken(channel string) (string, error) {
	if s.centrifuge.events != nil && s.centrifuge.events.onSubscriptionToken != nil {
		handler := s.centrifuge.events.onSubscriptionToken
		ev := SubscriptionTokenEvent{
			Channel: channel,
		}
		return handler(ev)
	}
	return "", errors.New("SubscriptionTokenHandler must be set to handle private Channel subscriptions")
}

// Lock must be held outside.
func (s *Subscription) scheduleSubRefresh(ttl uint32) {
	if s.state != SubStateSubscribed {
		return
	}
	s.refreshTimer = time.AfterFunc(time.Duration(ttl)*time.Second, func() {
		s.mu.Lock()
		if s.state != SubStateSubscribed {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		token, err := s.getSubscriptionToken(s.Channel)
		if err != nil {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.emitError(SubscriptionRefreshError{Err: err})
			s.scheduleSubRefresh(10)
			return
		}
		if token == "" {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.unsubscribe(unsubscribedUnauthorized, "unauthorized", true)
			return
		}

		s.centrifuge.sendSubRefresh(s.Channel, token, func(result *protocol.SubRefreshResult, err error) {
			if err != nil {
				s.emitError(SubscriptionSubscribeError{Err: err})
				var serverError *Error
				if errors.As(err, &serverError) {
					if serverError.Temporary {
						s.mu.Lock()
						defer s.mu.Unlock()
						s.scheduleSubRefresh(10)
						return
					} else {
						s.mu.Lock()
						defer s.mu.Unlock()
						s.unsubscribe(serverError.Code, serverError.Message, true)
						return
					}
				} else {
					s.mu.Lock()
					defer s.mu.Unlock()
					s.scheduleSubRefresh(10)
					return
				}
			}
			if result.Expires {
				s.mu.Lock()
				s.scheduleSubRefresh(result.Ttl)
				s.mu.Unlock()
			}
		})
	})
}
