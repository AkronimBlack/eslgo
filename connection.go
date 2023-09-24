/*
 * Copyright (c) 2020 Percipia
 *
 * This Source Code Form is subject to the terms of the Mozilla Public
 * License, v. 2.0. If a copy of the MPL was not distributed with this
 * file, You can obtain one at https://mozilla.org/MPL/2.0/.
 *
 * Contributor(s):
 * Andrew Querol <aquerol@percipia.com>
 */
package eslgo

import (
	"bufio"
	"context"
	"errors"
	"log"
	"net"
	"net/textproto"
	"sync"
	"time"

	"github.com/AkronimBlack/eslgo/command"
	"github.com/google/uuid"
)

/*Conn ...*/
type Conn struct {
	conn              net.Conn
	reader            *bufio.Reader
	header            *textproto.Reader
	writeLock         sync.Mutex
	runningContext    context.Context
	stopFunc          func()
	responseChannels  map[string]chan *RawResponse
	responseChanMutex sync.RWMutex
	eventListenerLock sync.RWMutex
	eventListeners    map[string]map[string]EventListener
	outbound          bool
	closeOnce         sync.Once
	finishedChannel   chan bool
}

func (c *Conn) FinishedChannel() chan bool {
	return c.finishedChannel
}

func (c *Conn) SetFinishedChannel(finishedChannel chan bool) {
	c.finishedChannel = finishedChannel
}

const EndOfMessage = "\r\n\r\n"

// NewConnection exported constructor for alternative builds
func NewConnection(c net.Conn, outbound bool) *Conn {
	reader := bufio.NewReader(c)
	header := textproto.NewReader(reader)

	runningContext, stop := context.WithCancel(context.Background())

	instance := &Conn{
		conn:   c,
		reader: reader,
		header: header,
		responseChannels: map[string]chan *RawResponse{
			TypeReply:       make(chan *RawResponse),
			TypeAPIResponse: make(chan *RawResponse),
			TypeEventPlain:  make(chan *RawResponse),
			TypeEventXML:    make(chan *RawResponse),
			TypeEventJSON:   make(chan *RawResponse),
			TypeAuthRequest: make(chan *RawResponse, 1), // Buffered to ensure we do not lose the initial auth request before we are setup to respond
			TypeDisconnect:  make(chan *RawResponse),
		},
		runningContext: runningContext,
		stopFunc:       stop,
		eventListeners: make(map[string]map[string]EventListener),
		outbound:       outbound,
	}
	go instance.receiveLoop()
	go instance.eventLoop()
	return instance
}

/*RegisterEventListener ... */
func (c *Conn) RegisterEventListener(channelUUID string, listener EventListener) string {
	c.eventListenerLock.Lock()
	defer c.eventListenerLock.Unlock()

	id := uuid.New().String()
	if _, ok := c.eventListeners[channelUUID]; ok {
		c.eventListeners[channelUUID][id] = listener
	} else {
		c.eventListeners[channelUUID] = map[string]EventListener{id: listener}
	}
	return id
}

/*RemoveEventListener .. */
func (c *Conn) RemoveEventListener(channelUUID string, id string) {
	c.eventListenerLock.Lock()
	defer c.eventListenerLock.Unlock()

	if listeners, ok := c.eventListeners[channelUUID]; ok {
		delete(listeners, id)
	}
}

func (c *Conn) SendCommand(ctx context.Context, command command.Command) (*RawResponse, error) {
	c.writeLock.Lock()
	defer c.writeLock.Unlock()

	if deadline, ok := ctx.Deadline(); ok {
		_ = c.conn.SetWriteDeadline(deadline)
	}
	_, err := c.conn.Write([]byte(command.BuildMessage() + EndOfMessage))
	if err != nil {
		return nil, err
	}

	// Get response
	c.responseChanMutex.RLock()
	defer c.responseChanMutex.RUnlock()
	select {
	case response := <-c.responseChannels[TypeReply]:
		if response == nil {
			// We only get nil here if the channel is closed
			return nil, errors.New("connection closed")
		}
		return response, nil
	case response := <-c.responseChannels[TypeAPIResponse]:
		if response == nil {
			// We only get nil here if the channel is closed
			return nil, errors.New("connection closed")
		}
		return response, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (c *Conn) ExitAndClose() {
	c.closeOnce.Do(func() {
		// Attempt a graceful closing of the connection with FreeSWITCH
		ctx, cancel := context.WithTimeout(c.runningContext, time.Second)
		_, _ = c.SendCommand(ctx, command.Exit{})
		cancel()
		c.close()
	})
}

func (c *Conn) Close() {
	c.closeOnce.Do(c.close)
}

func (c *Conn) close() {
	// Allow users to do anything they need to do before we tear everything down
	c.stopFunc()
	c.responseChanMutex.Lock()
	defer c.responseChanMutex.Unlock()
	for key, responseChan := range c.responseChannels {
		close(responseChan)
		delete(c.responseChannels, key)
	}
	if c.finishedChannel != nil {
		close(c.finishedChannel)
	}
	// Close the connection only after we have the response channel lock and we have deleted all response channels to ensure we don't receive on a closed channel
	_ = c.conn.Close()
}

func (c *Conn) callEventListener(event *Event) {
	c.eventListenerLock.RLock()
	defer c.eventListenerLock.RUnlock()

	// First check if there are any general event listener
	if listeners, ok := c.eventListeners[EventListenAll]; ok {
		for _, listener := range listeners {
			go listener(event)
		}
	}

	// Next call any listeners for a particular channel
	if event.HasHeader("Unique-Id") {
		channelUUID := event.GetHeader("Unique-Id")
		if listeners, ok := c.eventListeners[channelUUID]; ok {
			for _, listener := range listeners {
				go listener(event)
			}
		}
	}

	// Next call any listeners for a particular application
	if event.HasHeader("Application-UUID") {
		appUUID := event.GetHeader("Application-UUID")
		if listeners, ok := c.eventListeners[appUUID]; ok {
			for _, listener := range listeners {
				go listener(event)
			}
		}
	}

	// Next call any listeners for a particular job
	if event.HasHeader("Job-UUID") {
		jobUUID := event.GetHeader("Job-UUID")
		if listeners, ok := c.eventListeners[jobUUID]; ok {
			for _, listener := range listeners {
				go listener(event)
			}
		}
	}
}

func (c *Conn) eventLoop() {
	for {
		var event *Event
		var err error
		c.responseChanMutex.RLock()
		select {
		case raw := <-c.responseChannels[TypeEventPlain]:
			if raw == nil {
				// We only get nil here if the channel is closed
				c.responseChanMutex.RUnlock()
				return
			}
			event, err = readPlainEvent(raw.Body)
		case raw := <-c.responseChannels[TypeEventXML]:
			if raw == nil {
				// We only get nil here if the channel is closed
				c.responseChanMutex.RUnlock()
				return
			}
			event, err = readXMLEvent(raw.Body)
		case raw := <-c.responseChannels[TypeEventJSON]:
			if raw == nil {
				// We only get nil here if the channel is closed
				c.responseChanMutex.RUnlock()
				return
			}
			event, err = readJSONEvent(raw.Body)
		case <-c.runningContext.Done():
			c.responseChanMutex.RUnlock()
			return
		}
		c.responseChanMutex.RUnlock()

		if err != nil {
			log.Printf("Error parsing event\n%s\n", err.Error())
			continue
		}

		c.callEventListener(event)
	}
}

func (c *Conn) receiveLoop() {
	for c.runningContext.Err() == nil {
		disconnected, err := c.doMessage()
		if err != nil {
			log.Println("Error receiving message", err)
			break
		}
		if disconnected {
			c.stopFunc()
			if c.FinishedChannel() != nil {
				c.FinishedChannel() <- true
			}
			break
		}
	}
}

func (c *Conn) doMessage() (bool, error) {
	response, err := c.readResponse()
	if err != nil {
		return false, err
	}

	c.responseChanMutex.RLock()
	defer c.responseChanMutex.RUnlock()

	if response.GetHeader("Content-Type") == TypeDisconnect {
		return true, nil
	}

	responseChan, ok := c.responseChannels[response.GetHeader("Content-Type")]
	if !ok && len(c.responseChannels) <= 0 {
		// We must have shutdown!
		return false, errors.New("no response channels")
	}

	// We have a handler
	if ok {
		// Only allow 5 seconds to allow the handler to receive hte message on the channel
		ctx, cancel := context.WithTimeout(c.runningContext, 5*time.Second)
		defer cancel()

		select {
		case responseChan <- response:
		case <-c.runningContext.Done():
			// Parent connection context has stopped we most likely shutdown in the middle of waiting for a handler to handle the message
			return false, c.runningContext.Err()
		case <-ctx.Done():
			// Do not return an error since this is not fatal but log since it could be a indication of problems
			log.Printf("No one to handle response\nIs the connection overloaded or stopping?\n%v\n\n", response)
			c.stopFunc()
		}
	} else {
		return false, errors.New("no response channel for Content-Type: " + response.GetHeader("Content-Type"))
	}
	return false, nil
}
