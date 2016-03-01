package libcentrifugo

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/centrifugal/centrifugo/Godeps/_workspace/src/github.com/FZambia/go-logger"
	"gopkg.in/redis.v3"
)

const (
	// RedisSubscribeChannelSize is the size for the internal buffered channels RedisEngine
	// uses to synchronize subscribe/unsubscribe. It allows for effective batching during bulk re-subscriptions,
	// and allows large volume of incoming subscriptions to not block when PubSub connection is reconnecting.
	// Two channels of this size will be allocated, one for Subscribe and one for Unsubscribe
	RedisSubscribeChannelSize = 4096
	// Maximum number of channels to include in a single subscribe call. Redis documentation doesn't specify a
	// maximum allowed but we think it probably makes sense to keep a sane limit given how many subscriptions a single
	// Centrifugo instance might be handling
	RedisSubscribeBatchLimit = 2048
)

// RedisEngine uses Redis datastructures and PUB/SUB to manage Centrifugo logic.
// This engine allows to scale Centrifugo - you can run several Centrifugo instances
// connected to the same Redis and load balance clients between instances.
type RedisEngine struct {
	sync.RWMutex
	app          *Application
	client       *redis.Client
	api          bool
	numApiShards int
	subCh        chan subRequest
	unSubCh      chan subRequest
	pubScript    *redis.Script
}

// RedisEngineConfig is struct with Redis Engine options.
type RedisEngineConfig struct {
	// Host is Redis server host.
	Host string
	// Port is Redis server port.
	Port string
	// Password is password to use when connecting to Redis database. If empty then password not used.
	Password string
	// DB is Redis database number as string. If empty then database 0 used.
	DB string
	// URL to redis server in format redis://:password@hostname:port/db_number
	URL string
	// PoolSize is a size of Redis connection pool.
	PoolSize int
	// API enables listening for API queues to publish API commands into Centrifugo via pushing
	// commands into Redis queue.
	API bool
	// NumAPIShards is a number of sharded API queues in Redis to increase volume of commands
	// (most probably publish) that Centrifugo instance can process.
	NumAPIShards int
}

// subRequest is an internal request to subscribe or unsubscribe from one or more channels
type subRequest struct {
	Channel ChannelID
	err     *chan error
}

// newSubRequest creates a new request to subscribe or unsubscribe form a channel.
// If the caller cares about response they should set wantResponse and then call
// result() on the request once it has been pushed to the appropriate chan.
func newSubRequest(chID ChannelID, wantResponse bool) subRequest {
	r := subRequest{
		Channel: chID,
	}
	if wantResponse {
		eChan := make(chan error)
		r.err = &eChan
	}
	return r
}

func (sr *subRequest) done(err error) {
	if sr.err == nil {
		return
	}
	*(sr.err) <- err
}

func (sr *subRequest) result() error {
	if sr.err == nil {
		// No waiting, as caller didn't care about response
		return nil
	}
	return <-*(sr.err)
}

func yesno(condition bool) string {
	if condition {
		return "yes"
	}
	return "no"
}

// NewRedisEngine initializes Redis Engine.
func NewRedisEngine(app *Application, conf *RedisEngineConfig) *RedisEngine {
	host := conf.Host
	port := conf.Port
	password := conf.Password

	db := "0"
	if conf.DB != "" {
		db = conf.DB
	}

	// If URL set then prefer it over other parameters.
	if conf.URL != "" {
		u, err := url.Parse(conf.URL)
		if err != nil {
			logger.FATAL.Fatalln(err)
		}
		if u.User != nil {
			var ok bool
			password, ok = u.User.Password()
			if !ok {
				password = ""
			}
		}
		host, port, err = net.SplitHostPort(u.Host)
		if err != nil {
			logger.FATAL.Fatalln(err)
		}
		path := u.Path
		if path != "" {
			db = path[1:]
		}
	}

	server := host + ":" + port

	// pubScriptSource contains lua script we register in Redis to call when publishing
	// client message. It publishes message into channel and adds message to history
	// list maintaining history size and expiration time. This is an optimization to make
	// 1 round trip to Redis instead of 2.
	// KEYS[1] - history list key
	// ARGV[1] - channel to publish message to
	// ARGV[2] - message payload
	// ARGV[3] - history message payload
	// ARGV[4] - history size
	// ARGV[5] - history lifetime
	// ARGV[6] - history drop inactive flag - "0" or "1"
	pubScriptSource := `
local n = redis.call("publish", ARGV[1], ARGV[2])
local m = 0
if ARGV[6] == "1" and n == 0 then
  m = redis.call("lpushx", KEYS[1], ARGV[3])
else
  m = redis.call("lpush", KEYS[1], ARGV[3])
end
if m > 0 then
  redis.call("ltrim", KEYS[1], 0, ARGV[4])
  redis.call("expire", KEYS[1], ARGV[5])
end
return n
	`

	dbNum, err := strconv.Atoi(db)
	if err != nil {
		logger.FATAL.Fatalln("Can not convert redis db to number")
	}

	options := &redis.Options{
		Addr:         server,
		Password:     password,
		DB:           int64(dbNum),
		MaxRetries:   1,
		DialTimeout:  time.Second,
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
		PoolSize:     conf.PoolSize,
		PoolTimeout:  time.Second,
	}

	client := redis.NewClient(options)

	e := &RedisEngine{
		app:          app,
		client:       client,
		api:          conf.API,
		numApiShards: conf.NumAPIShards,
		pubScript:    redis.NewScript(pubScriptSource),
	}
	usingPassword := yesno(password != "")
	apiEnabled := yesno(conf.API)
	var shardsSuffix string
	if conf.API {
		shardsSuffix = fmt.Sprintf(", num shard queues: %d", conf.NumAPIShards)
	}
	logger.INFO.Printf("Redis engine: %s/%s, pool: %d, using password: %s, API enabled: %s%s\n", server, db, conf.PoolSize, usingPassword, apiEnabled, shardsSuffix)
	e.subCh = make(chan subRequest, RedisSubscribeChannelSize)
	e.unSubCh = make(chan subRequest, RedisSubscribeChannelSize)
	return e
}

func (e *RedisEngine) name() string {
	return "Redis"
}

func (e *RedisEngine) run() error {
	e.RLock()
	api := e.api
	e.RUnlock()
	go e.runForever(func() {
		e.runPubSub()
	})
	if api {
		go e.runForever(func() {
			e.runAPI()
		})
	}
	return nil
}

type redisAPIRequest struct {
	Data []apiCommand
}

// runForever simple keeps another function running indefinitely
// the reason this loop is not inside the function itself is so that defer
// can be used to cleanup nicely (defers only run at function return not end of block scope)
func (e *RedisEngine) runForever(fn func()) {
	for {
		fn()
	}
}

func (e *RedisEngine) runAPI() {

	e.app.RLock()
	apiKey := e.app.config.ChannelPrefix + "." + "api"
	e.app.RUnlock()

	done := make(chan struct{})
	defer close(done)

	popParams := []string{apiKey}
	workQueues := make(map[string]chan []byte)
	workQueues[apiKey] = make(chan []byte, 256)

	for i := 0; i < e.numApiShards; i++ {
		queueKey := fmt.Sprintf("%s.%d", apiKey, i)
		popParams = append(popParams, queueKey)
		workQueues[queueKey] = make(chan []byte, 256)
	}

	// Start a worker for each queue
	for name, ch := range workQueues {
		go func(name string, in <-chan []byte) {
			logger.INFO.Printf("Starting worker for API queue %s", name)
			for {
				select {
				case body, ok := <-in:
					if !ok {
						return
					}
					var req redisAPIRequest
					err := json.Unmarshal(body, &req)
					if err != nil {
						logger.ERROR.Println(err)
						continue
					}
					for _, command := range req.Data {
						_, err := e.app.apiCmd(command)
						if err != nil {
							logger.ERROR.Println(err)
						}
					}
				case <-done:
					return
				}
			}
		}(name, ch)
	}

	for {
		reply, err := e.client.BLPop(0, popParams...).Result()
		if err != nil {
			logger.ERROR.Println(err)
			return
		}

		println(reply)

		/*
			values, err := redis.Values(reply, nil)
			if err != nil {
				logger.ERROR.Println(err)
				return
			}
			if len(values) != 2 {
				logger.ERROR.Println("Wrong reply from Redis in BLPOP - expecting 2 values")
				continue
			}

			queue, okQ := values[0].([]byte)
			body, okVal := values[1].([]byte)
			if !okQ || !okVal {
				logger.ERROR.Println("Wrong reply from Redis in BLPOP - can not convert value")
				continue
			}

			// Pick worker based on queue
			q, ok := workQueues[string(queue)]
			if !ok {
				logger.ERROR.Println("Got message from a queue we didn't even know about!")
				continue
			}

			q <- body
		*/
	}
}

// fillBatchFromChan attempts to read items from a subRequest channel and append them to split
// until it either hits maxSize or would have to block. If batch is empty and chan is empty then
// batch might end up being zero length.
func fillBatchFromChan(ch <-chan subRequest, batch *[]subRequest, chIDs *[]string, maxSize int) {
	for len(*batch) < maxSize {
		select {
		case req := <-ch:
			*batch = append(*batch, req)
			*chIDs = append(*chIDs, string(req.Channel))
		default:
			return
		}
	}
}

func (e *RedisEngine) runPubSub() {

	pubsub, err := e.client.Subscribe("just-to-get-pubsub-now-need-a-better-way")
	if err != nil {
		logger.ERROR.Println(err)
		return
	}
	defer pubsub.Close()

	e.app.RLock()
	adminChannel := e.app.config.AdminChannel
	controlChannel := e.app.config.ControlChannel
	e.app.RUnlock()

	done := make(chan struct{})
	defer close(done)

	// Run subscriber routine
	go func() {
		logger.INFO.Println("Starting RedisEngine Subscriber")

		defer func() {
			logger.INFO.Println("Stopping RedisEngine Subscriber")
		}()
		for {
			select {
			case <-done:
				return
			case r := <-e.subCh:
				// Something to subscribe
				chIDs := []string{string(r.Channel)}
				batch := []subRequest{r}

				// Try to gather as many others as we can without waiting
				fillBatchFromChan(e.subCh, &batch, &chIDs, RedisSubscribeBatchLimit)
				// Send them all
				err := pubsub.Subscribe(chIDs...)
				if err != nil {
					// Subscribe error is fatal
					logger.ERROR.Printf("RedisEngine Subscriber error: %v\n", err)

					for i := range batch {
						batch[i].done(err)
					}

					return

					// Close conn, this should cause Receive to return with err below
					// and whole runPubSub method to restart
					// conn.Close()
					// return
				}
				for i := range batch {
					batch[i].done(nil)
				}
			case r := <-e.unSubCh:
				// Something to subscribe
				chIDs := []string{string(r.Channel)}
				batch := []subRequest{r}
				// Try to gather as many others as we can without waiting
				fillBatchFromChan(e.unSubCh, &batch, &chIDs, RedisSubscribeBatchLimit)
				// Send them all
				err := pubsub.Unsubscribe(chIDs...)
				if err != nil {
					// Subscribe error is fatal
					logger.ERROR.Printf("RedisEngine Unsubscriber error: %v\n", err)

					for i := range batch {
						batch[i].done(err)
					}

					return

					// Close conn, this should cause Receive to return with err below
					// and whole runPubSub method to restart
					//conn.Close()
					//return
				}
				for i := range batch {
					batch[i].done(nil)
				}
			}
		}
	}()

	// Subscribe to channels we need in bulk.
	// We don't care if they fail since conn will be closed and we'll retry
	// if they do anyway.
	// This saves a lot of allocating of pointless chans...
	r := newSubRequest(adminChannel, false)
	e.subCh <- r
	r = newSubRequest(controlChannel, false)
	e.subCh <- r
	for _, chID := range e.app.clients.channels() {
		r = newSubRequest(chID, false)
		e.subCh <- r
	}

	for {

		msgi, err := pubsub.Receive()
		if err != nil {
			logger.ERROR.Println(err)
			return
		}

		switch msg := msgi.(type) {
		case *redis.Message:
			e.app.handleMsg(ChannelID(msg.Channel), []byte(msg.Payload))
		case *redis.Subscription:
		default:
			logger.ERROR.Printf("RedisEngine Receiver error: unknown message %#v\n", msgi)
			return
		}
	}
}

func (e *RedisEngine) publish(chID ChannelID, message []byte, opts *publishOpts) error {
	var err error
	if opts == nil {
		// just publish message into channel.
		err = e.client.Publish(string(chID), string(message)).Err()
	} else {
		// publish message into channel and add history message.
		if opts.HistorySize > 0 && opts.HistoryLifetime > 0 {
			messageJSON, err := json.Marshal(opts.Message)
			if err != nil {
				logger.ERROR.Println(err)
				return nil
			}
			err = e.pubScript.Run(e.client, []string{e.getHistoryKey(chID)}, []string{string(chID), message, messageJSON, opts.HistorySize, opts.HistoryLifetime, opts.HistoryDropInactive}).Err()
		} else {
			err = e.client.Publish(chID, message).Err()
		}
	}
	return err
}

func (e *RedisEngine) subscribe(chID ChannelID) error {
	logger.DEBUG.Println("subscribe on Redis channel", chID)
	r := newSubRequest(chID, true)
	e.subCh <- r
	return r.result()
}

func (e *RedisEngine) unsubscribe(chID ChannelID) error {
	logger.DEBUG.Println("unsubscribe from Redis channel", chID)
	r := newSubRequest(chID, true)
	e.unSubCh <- r
	return r.result()
}

func (e *RedisEngine) getHashKey(chID ChannelID) string {
	e.app.RLock()
	defer e.app.RUnlock()
	return e.app.config.ChannelPrefix + ".presence.hash." + string(chID)
}

func (e *RedisEngine) getSetKey(chID ChannelID) string {
	e.app.RLock()
	defer e.app.RUnlock()
	return e.app.config.ChannelPrefix + ".presence.set." + string(chID)
}

func (e *RedisEngine) getHistoryKey(chID ChannelID) string {
	e.app.RLock()
	defer e.app.RUnlock()
	return e.app.config.ChannelPrefix + ".history.list." + string(chID)
}

func (e *RedisEngine) addPresence(chID ChannelID, uid ConnID, info ClientInfo) error {
	e.app.RLock()
	presenceExpireSeconds := e.app.config.PresenceExpireInterval.Seconds()
	e.app.RUnlock()
	infoJSON, err := json.Marshal(info)
	if err != nil {
		return err
	}
	expireAt := time.Now().Unix() + int64(presenceExpireSeconds)
	hashKey := e.getHashKey(chID)
	setKey := e.getSetKey(chID)

	tx, err := e.client.Watch(setKey, hashKey)
	if err != nil {
		return err
	}
	defer tx.Close()

	tx.ZAdd(setKey, expireAt, uid)
	tx.HSet(hashKey, uid, infoJSON)
	tx.Expire(setKey, presenceExpireSeconds)
	tx.Expire(hashKey, presenceExpireSeconds)

	_, err = tx.Exec(func() error {
		return nil
	})
	return err
}

func (e *RedisEngine) removePresence(chID ChannelID, uid ConnID) error {

	hashKey := e.getHashKey(chID)
	setKey := e.getSetKey(chID)

	tx, err := e.client.Watch(setKey, hashKey)
	if err != nil {
		return err
	}
	defer tx.Close()

	tx.HDel(hashKey, uid)
	tx.ZRem(setKey, uid)
	_, err = tx.Exec(func() error {
		return nil
	})
	return err
}

func mapStringClientInfo(result interface{}, err error) (map[ConnID]ClientInfo, error) {
	values, err := redis.Values(result, err)
	if err != nil {
		return nil, err
	}
	if len(values)%2 != 0 {
		return nil, errors.New("mapStringClientInfo expects even number of values result")
	}
	m := make(map[ConnID]ClientInfo, len(values)/2)
	for i := 0; i < len(values); i += 2 {
		key, okKey := values[i].([]byte)
		value, okValue := values[i+1].([]byte)
		if !okKey || !okValue {
			return nil, errors.New("ScanMap key not a bulk string value")
		}
		var f ClientInfo
		err = json.Unmarshal(value, &f)
		if err != nil {
			return nil, errors.New("can not unmarshal value to ClientInfo")
		}
		m[ConnID(key)] = f
	}
	return m, nil
}

func (e *RedisEngine) presence(chID ChannelID) (map[ConnID]ClientInfo, error) {

	now := time.Now().Unix()
	hashKey := e.getHashKey(chID)
	setKey := e.getSetKey(chID)
	opts := redis.ZRangeByScore{
		Min: 0,
		Max: now,
	}
	expiredKeys, err := e.client.ZRangeByScore(setKey, opts).Result()
	if err != nil {
		return nil, err
	}
	/*
		expiredKeys, err := redis.Strings(reply, nil)
		if err != nil {
			return nil, err
		}
	*/
	if len(expiredKeys) > 0 {

		tx, err := e.client.Watch(setKey, hashKey)
		if err != nil {
			return err
		}
		defer tx.Close()

		tx.ZRemRangeByScore(key, setKey, 0, now)
		for _, key := range expiredKeys {
			tx.HDel(hashKey, key)
		}
		_, err = tx.Exec(func() error {
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	reply, err := e.client.HGetAll(hashKey)
	if err != nil {
		return nil, err
	}
	return mapStringClientInfo(reply, nil)
}

func sliceOfMessages(result interface{}, err error) ([]Message, error) {
	values, err := redis.Values(result, err)
	if err != nil {
		return nil, err
	}
	msgs := make([]Message, len(values))
	for i := 0; i < len(values); i++ {
		value, okValue := values[i].([]byte)
		if !okValue {
			return nil, errors.New("error getting Message value")
		}
		var m Message
		err = json.Unmarshal(value, &m)
		if err != nil {
			return nil, errors.New("can not unmarshal value to Message")
		}
		msgs[i] = m
	}
	return msgs, nil
}

func (e *RedisEngine) history(chID ChannelID, opts historyOpts) ([]Message, error) {
	var rangeBound int = -1
	if opts.Limit > 0 {
		rangeBound = opts.Limit - 1 // Redis includes last index into result
	}
	historyKey := e.getHistoryKey(chID)
	reply, err := e.client.LRange(historyKey, 0, rangeBound)
	if err != nil {
		return nil, err
	}
	return sliceOfMessages(reply, nil)
}

func sliceOfChannelIDs(result interface{}, prefix string, err error) ([]ChannelID, error) {
	values, err := redis.Values(result, err)
	if err != nil {
		return nil, err
	}
	channels := make([]ChannelID, len(values))
	for i := 0; i < len(values); i++ {
		value, okValue := values[i].([]byte)
		if !okValue {
			return nil, errors.New("error getting ChannelID value")
		}
		chID := ChannelID(value)
		channels[i] = chID
	}
	return channels, nil
}

// Requires Redis >= 2.8.0 (http://redis.io/commands/pubsub)
func (e *RedisEngine) channels() ([]ChannelID, error) {
	prefix := e.app.channelIDPrefix()
	reply, err := e.client.PubSubChannels(prefix + "*")
	if err != nil {
		return nil, err
	}
	return sliceOfChannelIDs(reply, prefix, nil)
}
