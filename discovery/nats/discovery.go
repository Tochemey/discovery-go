/*
 * Copyright 2024 Arsene Tochemey Gandote
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package nats

import (
	"encoding/json"
	"errors"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/flowchartsman/retry"
	"github.com/nats-io/nats.go"
	"go.uber.org/atomic"

	"github.com/groupcache/discovery-go/discovery"
)

// Discovery represents the kubernetes discovery
type Discovery struct {
	config *Config
	mu     sync.Mutex

	initialized *atomic.Bool
	registered  *atomic.Bool

	// define the nats connection
	connection *nats.Conn
	// define a slice of subscriptions
	subscriptions []*nats.Subscription

	address string
}

// enforce compilation error
var _ discovery.Provider = &Discovery{}

// NewDiscovery returns an instance of the kubernetes discovery provider
func NewDiscovery(config *Config) *Discovery {
	// create an instance of
	discovery := &Discovery{
		mu:          sync.Mutex{},
		initialized: atomic.NewBool(false),
		registered:  atomic.NewBool(false),
		config:      config,
	}

	discovery.address = net.JoinHostPort(config.Host, strconv.Itoa(int(config.DiscoveryPort)))
	return discovery
}

// ID returns the discovery provider id
func (d *Discovery) ID() string {
	return "nats"
}

// Initialize initializes the plugin: registers some internal data structures, clients etc.
func (d *Discovery) Initialize() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.initialized.Load() {
		return discovery.ErrAlreadyInitialized
	}

	if err := d.config.Validate(); err != nil {
		return err
	}

	if d.config.Timeout <= 0 {
		d.config.Timeout = time.Second
	}

	if d.config.MaxJoinAttempts == 0 {
		d.config.MaxJoinAttempts = 5
	}

	if d.config.ReconnectWait <= 0 {
		d.config.ReconnectWait = 2 * time.Second
	}

	// create the nats connection option
	opts := nats.GetDefaultOptions()
	opts.Url = d.config.Server
	opts.Name = net.JoinHostPort(d.config.Host, strconv.Itoa(int(d.config.DiscoveryPort)))
	opts.ReconnectWait = d.config.ReconnectWait
	opts.MaxReconnect = -1

	var (
		connection *nats.Conn
		err        error
	)

	// let us connect using an exponential backoff mechanism
	// create a new instance of retrier that will try a maximum of five times, with
	// an initial delay of 100 ms and a maximum delay of opts.ReconnectWait
	err = retry.
		NewRetrier(d.config.MaxJoinAttempts, 100*time.Millisecond, opts.ReconnectWait).
		Run(func() error {
			connection, err = opts.Connect()
			if err != nil {
				return err
			}
			return nil
		})

	// create the NATs connection
	d.connection = connection
	d.initialized = atomic.NewBool(true)
	return nil
}

// Register registers this node to a service discovery directory.
func (d *Discovery) Register() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.registered.Load() {
		return discovery.ErrAlreadyRegistered
	}

	// create the subscription handler
	handler := func(msg *nats.Msg) {
		message := new(Message)
		if err := json.Unmarshal(msg.Data, message); err != nil {
			// TODO: need to read more and see how to propagate the error
			d.connection.Opts.AsyncErrorCB(d.connection, msg.Sub, errors.New("nats: Got an error trying to unmarshal: "+err.Error()))
			return
		}

		switch message.Type {
		case Deregister:
			// pass
		case Register:
			// pass
		case Request:
			response := &Message{
				Host: d.config.Host,
				Port: d.config.DiscoveryPort,
				Name: net.JoinHostPort(d.config.Host, strconv.Itoa(int(d.config.DiscoveryPort))),
				Type: Response,
			}

			bytea, _ := json.Marshal(response)
			if err := d.connection.Publish(msg.Reply, bytea); err != nil {
				// TODO: add a logger or panic
			}
		}
	}

	subscription, err := d.connection.Subscribe(d.config.Subject, handler)
	if err != nil {
		return err
	}

	d.subscriptions = append(d.subscriptions, subscription)
	d.registered = atomic.NewBool(true)
	return nil
}

// Deregister removes this node from a service discovery directory.
func (d *Discovery) Deregister() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	// first check whether the discovery provider has been registered or not
	if !d.registered.Load() {
		return discovery.ErrNotRegistered
	}

	// shutdown all the subscriptions
	for _, subscription := range d.subscriptions {
		// when subscription is defined
		if subscription != nil {
			// check whether the subscription is active or not
			if subscription.IsValid() {
				// unsubscribe and return when there is an error
				if err := subscription.Unsubscribe(); err != nil {
					return err
				}
			}
		}
	}

	// send the de-registration message to notify peers
	if d.connection != nil {
		// send a message to deregister stating we are out
		message := &Message{
			Host: d.config.Host,
			Port: d.config.DiscoveryPort,
			Name: net.JoinHostPort(d.config.Host, strconv.Itoa(int(d.config.DiscoveryPort))),
			Type: Deregister,
		}

		bytea, _ := json.Marshal(message)
		return d.connection.Publish(d.config.Subject, bytea)
	}
	d.registered.Store(false)
	return nil
}

// DiscoverPeers returns a list of known nodes.
func (d *Discovery) DiscoverPeers() ([]string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if !d.initialized.Load() {
		return nil, discovery.ErrNotInitialized
	}

	if !d.registered.Load() {
		return nil, discovery.ErrNotRegistered
	}

	// Set up a reply channel, then broadcast for all peers to
	// report their presence.
	// collect as many responses as possible in the given timeout.
	inbox := nats.NewInbox()
	msgCh := make(chan *nats.Msg, 1)

	// bind to receive messages
	sub, err := d.connection.ChanSubscribe(inbox, msgCh)
	if err != nil {
		return nil, err
	}

	request := &Message{
		Host: d.config.Host,
		Port: d.config.DiscoveryPort,
		Name: net.JoinHostPort(d.config.Host, strconv.Itoa(int(d.config.DiscoveryPort))),
		Type: Request,
	}

	bytea, _ := json.Marshal(request)
	if err = d.connection.PublishRequest(d.config.Subject, inbox, bytea); err != nil {
		return nil, err
	}

	var peers []string
	timeout := time.After(d.config.Timeout)
	me := net.JoinHostPort(d.config.Host, strconv.Itoa(int(d.config.DiscoveryPort)))
	for {
		select {
		case msg, ok := <-msgCh:
			if !ok {
				// Subscription is closed
				return peers, nil
			}

			message := new(Message)
			if err := json.Unmarshal(msg.Data, message); err != nil {
				return nil, err
			}

			// get the found peer address
			addr := net.JoinHostPort(message.Host, strconv.Itoa(int(message.Port)))
			if addr == me {
				continue
			}

			peers = append(peers, addr)

		case <-timeout:
			_ = sub.Unsubscribe()
			close(msgCh)
		}
	}
}

// Close closes the provider
func (d *Discovery) Close() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.initialized.Store(false)
	d.registered.Store(false)

	if d.connection != nil {
		defer func() {
			d.connection.Close()
			d.connection = nil
		}()

		for _, subscription := range d.subscriptions {
			if subscription != nil {
				if subscription.IsValid() {
					if err := subscription.Unsubscribe(); err != nil {
						return err
					}
				}
			}
		}

		return d.connection.Flush()
	}
	return nil
}