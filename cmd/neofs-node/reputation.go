package main

import (
	"bytes"
	"encoding/hex"

	crypto "github.com/nspcc-dev/neofs-crypto"
	netmapcore "github.com/nspcc-dev/neofs-node/pkg/core/netmap"
	"github.com/nspcc-dev/neofs-node/pkg/morph/event"
	"github.com/nspcc-dev/neofs-node/pkg/morph/event/netmap"
	"github.com/nspcc-dev/neofs-node/pkg/services/reputation"
	trustcontroller "github.com/nspcc-dev/neofs-node/pkg/services/reputation/local/controller"
	truststorage "github.com/nspcc-dev/neofs-node/pkg/services/reputation/local/storage"
	"github.com/nspcc-dev/neofs-node/pkg/util/logger"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type localTrustStorage struct {
	log *logger.Logger

	storage *truststorage.Storage

	nmSrc netmapcore.Source

	localKey []byte
}

type localTrustIterator struct {
	ctx trustcontroller.Context

	storage *localTrustStorage

	epochStorage *truststorage.EpochTrustValueStorage
}

func (s *localTrustStorage) InitIterator(ctx trustcontroller.Context) (trustcontroller.Iterator, error) {
	epochStorage, err := s.storage.DataForEpoch(ctx.Epoch())
	if err != nil && !errors.Is(err, truststorage.ErrNoPositiveTrust) {
		return nil, err
	}

	return &localTrustIterator{
		ctx:          ctx,
		storage:      s,
		epochStorage: epochStorage,
	}, nil
}

func (it *localTrustIterator) Iterate(h reputation.TrustHandler) error {
	if it.epochStorage != nil {
		err := it.epochStorage.Iterate(h)
		if !errors.Is(err, truststorage.ErrNoPositiveTrust) {
			return err
		}
	}

	nm, err := it.storage.nmSrc.GetNetMapByEpoch(it.ctx.Epoch())
	if err != nil {
		return err
	}

	// find out if local node is presented in netmap
	localIndex := -1

	for i := range nm.Nodes {
		if bytes.Equal(nm.Nodes[i].PublicKey(), it.storage.localKey) {
			localIndex = i
		}
	}

	ln := len(nm.Nodes)
	if localIndex >= 0 && ln > 0 {
		ln--
	}

	// calculate Pj http://ilpubs.stanford.edu:8090/562/1/2002-56.pdf Chapter 4.5.
	p := reputation.TrustOne.Div(reputation.TrustValueFromInt(ln))

	for i := range nm.Nodes {
		if i == localIndex {
			continue
		}

		trust := reputation.Trust{}
		trust.SetPeer(reputation.PeerIDFromBytes(nm.Nodes[i].PublicKey()))
		trust.SetValue(p)

		if err := h(trust); err != nil {
			return err
		}
	}

	return nil
}

func (s *localTrustStorage) InitWriter(ctx trustcontroller.Context) (trustcontroller.Writer, error) {
	return &localTrustLogger{
		ctx: ctx,
		log: s.log,
	}, nil
}

type localTrustLogger struct {
	ctx trustcontroller.Context

	log *logger.Logger
}

func (l *localTrustLogger) Write(t reputation.Trust) error {
	l.log.Info("new local trust",
		zap.Uint64("epoch", l.ctx.Epoch()),
		zap.String("peer", hex.EncodeToString(t.Peer().Bytes())),
		zap.Stringer("value", t.Value()),
	)

	return nil
}

func (*localTrustLogger) Close() error {
	return nil
}

func initReputationService(c *cfg) {
	// consider sharing this between application components
	nmSrc := newCachedNetmapStorage(c.cfgNetmap.state, c.cfgNetmap.wrapper)

	c.cfgReputation.localTrustStorage = truststorage.New(truststorage.Prm{})

	trustStorage := &localTrustStorage{
		log:      c.log,
		storage:  c.cfgReputation.localTrustStorage,
		nmSrc:    nmSrc,
		localKey: crypto.MarshalPublicKey(&c.key.PublicKey),
	}

	c.cfgReputation.localTrustCtrl = trustcontroller.New(trustcontroller.Prm{
		LocalTrustSource: trustStorage,
		LocalTrustTarget: trustStorage,
	})

	addNewEpochNotificationHandler(c, func(ev event.Event) {
		var reportPrm trustcontroller.ReportPrm

		// report collected values from previous epoch
		reportPrm.SetEpoch(ev.(netmap.NewEpoch).EpochNumber() - 1)

		// TODO: implement and use worker pool [neofs-node#440]
		go c.cfgReputation.localTrustCtrl.Report(reportPrm)
	})
}
