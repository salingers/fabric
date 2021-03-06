/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

                 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package broadcast

import (
	"github.com/hyperledger/fabric/orderer/common/filter"
	cb "github.com/hyperledger/fabric/protos/common"
	ab "github.com/hyperledger/fabric/protos/orderer"
	"github.com/op/go-logging"

	"io"

	"github.com/golang/protobuf/proto"
)

var logger = logging.MustGetLogger("orderer/common/broadcast")

// ConfigUpdateProcessor is used to transform CONFIG_UPDATE transactions which are used to generate other envelope
// message types with preprocessing by the orderer
type ConfigUpdateProcessor interface {
	// Process takes in an envelope of type CONFIG_UPDATE and proceses it
	// to transform it either into another envelope type
	Process(envConfigUpdate *cb.Envelope) (*cb.Envelope, error)
}

// Handler defines an interface which handles broadcasts
type Handler interface {
	// Handle starts a service thread for a given gRPC connection and services the broadcast connection
	Handle(srv ab.AtomicBroadcast_BroadcastServer) error
}

// SupportManager provides a way for the Handler to look up the Support for a chain
type SupportManager interface {
	ConfigUpdateProcessor

	// GetChain gets the chain support for a given ChannelId
	GetChain(chainID string) (Support, bool)
}

// Support provides the backing resources needed to support broadcast on a chain
type Support interface {
	// Enqueue accepts a message and returns true on acceptance, or false on shutdown
	Enqueue(env *cb.Envelope) bool

	// Filters returns the set of broadcast filters for this chain
	Filters() *filter.RuleSet
}

type handlerImpl struct {
	sm SupportManager
}

// NewHandlerImpl constructs a new implementation of the Handler interface
func NewHandlerImpl(sm SupportManager) Handler {
	return &handlerImpl{
		sm: sm,
	}
}

// Handle starts a service thread for a given gRPC connection and services the broadcast connection
func (bh *handlerImpl) Handle(srv ab.AtomicBroadcast_BroadcastServer) error {
	for {
		msg, err := srv.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		payload := &cb.Payload{}
		err = proto.Unmarshal(msg.Payload, payload)
		if payload.Header == nil || payload.Header.ChannelHeader == nil || payload.Header.ChannelHeader.ChannelId == "" {
			logger.Debugf("Received malformed message, dropping connection")
			return srv.Send(&ab.BroadcastResponse{Status: cb.Status_BAD_REQUEST})
		}

		if payload.Header.ChannelHeader.Type == int32(cb.HeaderType_CONFIG_UPDATE) {
			logger.Debugf("Preprocessing CONFIG_UPDATE")
			msg, err = bh.sm.Process(msg)
			if err != nil {
				return srv.Send(&ab.BroadcastResponse{Status: cb.Status_BAD_REQUEST})
			}

			err = proto.Unmarshal(msg.Payload, payload)
			if payload.Header == nil || payload.Header.ChannelHeader == nil || payload.Header.ChannelHeader.ChannelId == "" {
				logger.Criticalf("Generated bad transaction after CONFIG_UPDATE processing")
				return srv.Send(&ab.BroadcastResponse{Status: cb.Status_INTERNAL_SERVER_ERROR})
			}
		}

		support, ok := bh.sm.GetChain(payload.Header.ChannelHeader.ChannelId)
		if !ok {
			return srv.Send(&ab.BroadcastResponse{Status: cb.Status_NOT_FOUND})
		}

		if logger.IsEnabledFor(logging.DEBUG) {
			logger.Debugf("Broadcast is filtering message for channel %s", payload.Header.ChannelHeader.ChannelId)
		}

		// Normal transaction for existing chain
		_, filterErr := support.Filters().Apply(msg)

		if filterErr != nil {
			logger.Debugf("Rejecting broadcast message")
			return srv.Send(&ab.BroadcastResponse{Status: cb.Status_BAD_REQUEST})
		}

		if !support.Enqueue(msg) {
			logger.Debugf("Consenter instructed us to shut down")
			return srv.Send(&ab.BroadcastResponse{Status: cb.Status_SERVICE_UNAVAILABLE})
		}

		if logger.IsEnabledFor(logging.DEBUG) {
			logger.Debugf("Broadcast is successfully enqueued message for chain %s", payload.Header.ChannelHeader.ChannelId)
		}

		err = srv.Send(&ab.BroadcastResponse{Status: cb.Status_SUCCESS})

		if err != nil {
			return err
		}
	}
}
