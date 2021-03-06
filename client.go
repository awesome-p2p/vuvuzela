// Copyright 2015 David Lazar. All rights reserved.
// Use of this source code is governed by the GNU AGPL
// license that can be found in the LICENSE file.

package vuvuzela

import (
	"fmt"
	"sync"

	"github.com/davidlazar/go-crypto/encoding/base32"
	"golang.org/x/crypto/ed25519"

	"vuvuzela.io/alpenhorn/config"
	"vuvuzela.io/alpenhorn/errors"
	"vuvuzela.io/alpenhorn/log"
	"vuvuzela.io/alpenhorn/typesocket"
	"vuvuzela.io/crypto/onionbox"
	"vuvuzela.io/vuvuzela/convo"
	"vuvuzela.io/vuvuzela/coordinator"
	"vuvuzela.io/vuvuzela/mixnet"
)

type Client struct {
	PersistPath string

	ConfigClient *config.Client
	Handler      ConvoHandler

	conn typesocket.Conn

	mu     sync.Mutex
	rounds map[uint32]*roundState

	convoConfig     *config.SignedConfig
	convoConfigHash string
}

type roundState struct {
	OnionKeys    [][]*[32]byte
	Config       *convo.ConvoConfig
	ConfigParent *config.SignedConfig
}

type ConvoHandler interface {
	Outgoing(round uint32) []*convo.DeadDropMessage
	Replies(round uint32, messages [][]byte)
}

func (c *Client) Connect() error {
	// TODO check if already connected
	if c.Handler == nil {
		return fmt.Errorf("no convo handler")
	}

	if c.rounds == nil {
		c.rounds = make(map[uint32]*roundState)
	}

	// Fetch the current config to get the coordinator's key and address.
	convoConfig, err := c.ConfigClient.CurrentConfig("Convo")
	if err != nil {
		return errors.Wrap(err, "fetching latest convo config")
	}
	convoInner := convoConfig.Inner.(*convo.ConvoConfig)

	wsAddr := fmt.Sprintf("wss://%s/convo/ws", convoInner.Coordinator.Address)
	conn, err := typesocket.Dial(wsAddr, convoInner.Coordinator.Key, c.convoMux())
	if err != nil {
		return err
	}
	c.conn = conn

	return nil
}

func (c *Client) convoMux() typesocket.Mux {
	return typesocket.NewMux(map[string]interface{}{
		"newround": c.newConvoRound,
		"mix":      c.sendConvoOnion,
		"reply":    c.openReplyOnion,
		"error":    c.convoRoundError,
	})
}

func (c *Client) convoRoundError(conn typesocket.Conn, v coordinator.RoundError) {
	log.WithFields(log.Fields{"round": v.Round}).Errorf("convo coordinator sent error: %s", v.Err)
}

func (c *Client) newConvoRound(conn typesocket.Conn, v coordinator.NewRound) {
	c.mu.Lock()
	defer c.mu.Unlock()

	st, ok := c.rounds[v.Round]
	if ok {
		if st.ConfigParent.Hash() != v.ConfigHash {
			log.Errorf("%s", errors.New("coordinator announced different configs round %d", v.Round))
		}
		return
	}

	// common case
	if v.ConfigHash == c.convoConfigHash {
		c.rounds[v.Round] = &roundState{
			Config:       c.convoConfig.Inner.(*convo.ConvoConfig),
			ConfigParent: c.convoConfig,
		}
		return
	}

	configs, err := c.ConfigClient.FetchAndVerifyChain(c.convoConfig, v.ConfigHash)
	if err != nil {
		log.Errorf("%s", errors.Wrap(err, "fetching convo config"))
		return
	}

	newConfig := configs[0]
	c.convoConfig = newConfig
	c.convoConfigHash = v.ConfigHash

	if err := c.persistLocked(); err != nil {
		panic("failed to persist state: " + err.Error())
	}

	c.rounds[v.Round] = &roundState{
		Config:       newConfig.Inner.(*convo.ConvoConfig),
		ConfigParent: newConfig,
	}
}

func (c *Client) sendConvoOnion(conn typesocket.Conn, v coordinator.MixRound) {
	round := v.MixSettings.Round

	c.mu.Lock()
	st, ok := c.rounds[round]
	c.mu.Unlock()
	if !ok {
		err := errors.New("sendConvoOnion: round %d not configured", round)
		log.Errorf("%s", err)
		return
	}

	settingsMsg := v.MixSettings.SigningMessage()

	for i, mixer := range st.Config.MixServers {
		if !ed25519.Verify(mixer.Key, settingsMsg, v.MixSignatures[i]) {
			err := errors.New(
				"round %d: failed to verify mixnet settings for key %s",
				round, base32.EncodeToString(mixer.Key),
			)
			log.Errorf("%s", err)
			return
		}
	}

	outgoing := c.Handler.Outgoing(round)
	onionKeys := make([][]*[32]byte, len(outgoing))
	onions := make([][]byte, len(outgoing))
	for i, deadDropMsg := range outgoing {
		msg := deadDropMsg.Marshal()
		onions[i], onionKeys[i] = onionbox.Seal(msg, mixnet.ForwardNonce(round), v.MixSettings.OnionKeys)
	}

	c.mu.Lock()
	st.OnionKeys = onionKeys
	c.mu.Unlock()

	conn.Send("onion", coordinator.OnionMsg{
		Round:  round,
		Onions: onions,
	})
}

func (c *Client) openReplyOnion(conn typesocket.Conn, v coordinator.OnionMsg) {
	c.mu.Lock()
	st, ok := c.rounds[v.Round]
	c.mu.Unlock()
	if !ok {
		log.WithFields(log.Fields{"round": v.Round}).Error("round not found")
		return
	}

	if len(st.OnionKeys) != len(v.Onions) {
		log.WithFields(log.Fields{"round": v.Round}).Errorf("expected %d onions, got %d", len(st.OnionKeys), len(v.Onions))
		return
	}

	msgs := make([][]byte, len(v.Onions))
	for i, onion := range v.Onions {
		msg, ok := onionbox.Open(onion, mixnet.BackwardNonce(v.Round), st.OnionKeys[i])
		if !ok {
			log.WithFields(log.Fields{"round": v.Round}).Error("failed to decrypt onion")
		}
		msgs[i] = msg
	}

	c.Handler.Replies(v.Round, msgs)

	c.mu.Lock()
	delete(c.rounds, v.Round)
	c.mu.Unlock()
}
