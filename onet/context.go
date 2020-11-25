package onet

import (
	"github.com/cypherium/cypherBFT/onet/network"
)

// Context represents the methods that are available to a service.
type Context struct {
	server     *Server
	serviceID  ServiceID
	manager    *serviceManager
	bucketName []byte
}

// defaultContext is the implementation of the Context interface. It is
// instantiated for each Service.
func newContext(c *Server, servID ServiceID, manager *serviceManager) *Context {
	return &Context{
		server:     c,
		serviceID:  servID,
		manager:    manager,
		bucketName: []byte(ServiceFactory.Name(servID)),
	}
}

func (c *Context) SendRaw1(ps ...interface{}) {
	si := ps[0].(*network.ServerIdentity)
	msg := ps[1]
	bForeConnect := ps[2].(bool)
	c.server.Send(si, msg, bForeConnect)
}

// SendRaw sends a message to the ServerIdentity.
func (c *Context) SendRaw(si *network.ServerIdentity, msg interface{}, bForeConnect bool) error {
	_, err := c.server.Send(si, msg, bForeConnect)
	return err
}

// ServerIdentity returns this server's identity.
func (c *Context) ServerIdentity() *network.ServerIdentity {
	return c.server.ServerIdentity
}

// ServiceID returns the service-id.
func (c *Context) ServiceID() ServiceID {
	return c.serviceID
}

// RegisterProcessor overrides the RegisterProcessor methods of the Dispatcher.
// It delegates the dispatching to the serviceManager.
func (c *Context) RegisterProcessor(p network.Processor, msgType network.MessageTypeID) {
	c.manager.registerProcessor(p, msgType)
}

// RegisterProcessorFunc takes a message-type and a function that will be called
// if this message-type is received.
func (c *Context) RegisterProcessorFunc(msgType network.MessageTypeID, fn func(*network.Envelope)) {
	c.manager.registerProcessorFunc(msgType, fn)
}

// Service returns the corresponding service.
func (c *Context) Service(name string) Service {
	return c.manager.service(name)
}

// String returns the host it's running on.
func (c *Context) String() string {
	return c.server.ServerIdentity.String()
}
