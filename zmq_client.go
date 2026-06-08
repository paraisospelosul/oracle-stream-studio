package main

import (
	"context"
	"fmt"
	"sync"

	"github.com/go-zeromq/zmq4"
)

type ZMQClient struct {
	addr   string
	socket zmq4.Socket
	mu     sync.Mutex
}

func NewZMQClient(addr string) *ZMQClient {
	return &ZMQClient{addr: addr}
}

func (c *ZMQClient) Connect() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.socket = zmq4.NewReq(context.Background())
	err := c.socket.Dial(c.addr)
	if err != nil {
		return err
	}
	return nil
}

func (c *ZMQClient) SendCommand(target, command, arg string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.socket == nil {
		return "", fmt.Errorf("ZMQ socket not connected")
	}

	// Format for FFmpeg zmq filter: "Parsed_overlay_0 x 100" -> target command arg
	msg := fmt.Sprintf("%s %s %s", target, command, arg)
	
	err := c.socket.Send(zmq4.NewMsgString(msg))
	if err != nil {
		return "", err
	}

	reply, err := c.socket.Recv()
	if err != nil {
		return "", err
	}

	return string(reply.Frames[0]), nil
}

func (c *ZMQClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.socket != nil {
		c.socket.Close()
		c.socket = nil
	}
}
