package gilmour

import (
	"errors"
	"fmt"
	"gopkg.in/gilmour-libs/gilmour-go.v0/protocol"
	"sync"
	"time"
)

func Get(backend Backend) *Gilmour {
	x := Gilmour{}
	x.subscribers = map[string][]*Subscription{}
	x.addBackend(backend)
	return &x
}

type Gilmour struct {
	enableHealthCheck bool
	identMutex        sync.RWMutex
	subscriberMutex   sync.RWMutex
	backend           Backend
	ident             string
	errorMethod       string
	subscribers       map[string][]*Subscription
}

func (self *Gilmour) addBackend(backend Backend) {
	self.backend = backend
}

func (self *Gilmour) GetIdent() string {
	self.identMutex.Lock()
	defer self.identMutex.Unlock()

	if self.ident == protocol.BLANK {
		self.ident = protocol.MakeIdent()
	}

	return self.ident
}

func (self *Gilmour) registerIdent() {
	ident := self.GetIdent()
	err := self.backend.RegisterIdent(ident)
	if err != nil {
		panic(err)
	}
}

func (self *Gilmour) unregisterIdent() {
	if !self.IsHealthCheckEnabled() {
		return
	}

	ident := self.GetIdent()
	err := self.backend.UnregisterIdent(ident)
	if err != nil {
		panic(err)
	}
}

func (self *Gilmour) IsHealthCheckEnabled() bool {
	return self.enableHealthCheck
}

func (self *Gilmour) SetHealthCheckEnabled() *Gilmour {
	self.enableHealthCheck = true
	subscribeHealth(self)
	self.registerIdent()
	return self
}

func (self *Gilmour) removeSubscribers(topic string) (err error) {
	self.subscriberMutex.Lock()
	defer self.subscriberMutex.Unlock()

	self.subscribers[topic] = []*Subscription{}
	return
}

func (self *Gilmour) removeSubscriber(topic string, s *Subscription) (err error) {
	self.subscriberMutex.Lock()
	defer self.subscriberMutex.Unlock()

	list, ok := self.subscribers[topic]
	if !ok {
		err = errors.New("Subscribers list is already empty")
		return
	}

	new_list := []*Subscription{}

	for _, elem := range list {
		if elem == s {
			//Do nothing
			continue
		}

		new_list = append(new_list, elem)
	}

	self.subscribers[topic] = new_list
	if len(new_list) == 0 {
		delete(self.subscribers, topic)
	}

	return
}

func (self *Gilmour) addSubscriber(topic string, h Handler, opts *HandlerOpts) *Subscription {
	self.subscriberMutex.Lock()
	defer self.subscriberMutex.Unlock()

	if _, ok := self.subscribers[topic]; !ok {
		self.subscribers[topic] = []*Subscription{}
	}

	sub := &Subscription{h, opts}

	arr := self.subscribers[topic]
	arr = append(arr, sub)
	self.subscribers[topic] = arr

	return sub
}

func (self *Gilmour) Subscribe(topic string, h Handler, opts *HandlerOpts) *Subscription {
	if _, ok := self.subscribers[topic]; !ok {
		self.backend.Subscribe(topic)
	}

	return self.addSubscriber(topic, h, opts)
}

func (self *Gilmour) Unsubscribe(topic string, s *Subscription) {
	err := self.removeSubscriber(topic, s)
	if err != nil {
		panic(err)
	}

	if _, ok := self.subscribers[topic]; !ok {
		err := self.backend.Unsubscribe(topic)
		if err != nil {
			panic(err)
		}
	}
}

func (self *Gilmour) UnsubscribeAll(topic string) {
	self.removeSubscribers(topic)
	err := self.backend.Unsubscribe(topic)
	if err != nil {
		panic(err)
	}
}

func (self *Gilmour) CanReportErrors() bool {
	return self.errorMethod != protocol.BLANK
}

func (self *Gilmour) SetErrorMethod(method string) {
	if method != protocol.QUEUE && method != protocol.PUBLISH && method != protocol.BLANK {
		panic(errors.New(fmt.Sprintf(
			"error method can only be %v, %v or %v",
			protocol.QUEUE, protocol.PUBLISH, protocol.BLANK,
		)))
	}

	self.errorMethod = method
}

func (self *Gilmour) GetErrorMethod() string {
	return self.errorMethod
}

func (self *Gilmour) ReportError(message *protocol.Error) {
	err := self.backend.ReportError(self.GetErrorMethod(), message)
	if err != nil {
		panic(err)
	}
}

func (self *Gilmour) Publish(topic string, opts *Publisher) string {
	//Publish the message

	sender := protocol.MakeSenderId()
	//Always generate a senderId for the message being sent out

	if opts.GetHandler() != nil {
		//If a handler is being supplied, subscribe to a response
		respChannel := self.backend.ResponseTopic(sender)
		//Wait for a responseHandler
		handlerOpts := MakeHandlerOpts().SetOneShot().SetSendResponse(false)
		self.Subscribe(respChannel, opts.GetHandler(), handlerOpts)
	}

	if opts.GetCode() == 0 {
		opts.SetCode(200)
	}

	err := self.backend.Publish(topic, opts.ToSentRequest(sender))
	if err != nil {
		panic(err)
	}

	return sender
}

func (self *Gilmour) processMessage(msg *protocol.Message) {
	subs, ok := self.subscribers[msg.Key]
	if !ok || len(subs) == 0 {
		fmt.Println("No subs found!! Key: " + msg.Key)
		return
	}

	for _, s := range subs {
		if s.GetOpts() != nil && s.GetOpts().IsOneShot() {
			fmt.Println("Message to", msg.Key, msg.Topic, "is one shot")
			self.Unsubscribe(msg.Key, s)
		}

		self.executeSubscriber(s, msg.Topic, msg.Data)
	}
}

func (self *Gilmour) executeSubscriber(s *Subscription, topic string, data interface{}) {
	d, err := protocol.ParseResponse(data)
	if err != nil {
		panic(err)
	}

	opts := s.GetOpts()
	if opts.GetGroup() != protocol.BLANK &&
		!self.backend.AcquireGroupLock(opts.GetGroup(), d.GetSender()) {
		fmt.Println("Cannot acquire Lock to process request.")
		return
	}

	go self.handleRequest(s, topic, d)
}

func (self *Gilmour) handleRequest(s *Subscription, topic string, d *protocol.RecvRequest) {
	req := NewRequest(topic, d)
	res := NewResponse(self.backend.ResponseTopic(d.GetSender()))

	done := make(chan bool, 1)

	//Executing Request
	go func(done chan<- bool) {
		s.GetHandler()(req, res)
		done <- true
	}(done)

	time.AfterFunc(time.Duration(s.GetOpts().GetTimeout())*time.Second, func() {
		done <- false
	})

	status := <-done

	if s.GetOpts().ShouldSendResponse() {
		err := res.Send()
		if err != nil {
			panic(err)
		}

		opts := NewPublisher().SetData(res.message).SetCode(res.code)

		if status == false {
			opts.SetCode(499).SetData("Execution timed out")
		}

		self.Publish(res.senderchannel, opts)
	}
}

func (self *Gilmour) addConsumer(sink <-chan *protocol.Message) {
	for {
		msg := <-sink
		go self.processMessage(msg)
	}
}

func (self *Gilmour) Start() {
	sink := self.backend.Start()
	go self.addConsumer(sink)
	bindSignals(self)
}

func (self *Gilmour) Stop() {
	defer self.unregisterIdent()
	defer self.backend.Stop()

	for topic, _ := range self.subscribers {
		self.UnsubscribeAll(topic)
	}
}
