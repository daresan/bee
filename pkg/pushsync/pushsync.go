// Copyright 2020 The Swarm Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pushsync

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/ethersphere/bee/pkg/logging"
	"github.com/ethersphere/bee/pkg/p2p"
	"github.com/ethersphere/bee/pkg/p2p/protobuf"
	"github.com/ethersphere/bee/pkg/pushsync/pb"
	"github.com/ethersphere/bee/pkg/storage"
	"github.com/ethersphere/bee/pkg/swarm"
	"github.com/ethersphere/bee/pkg/topology"
)

const (
	protocolName    = "pushsync"
	protocolVersion = "1.0.0"
	streamName      = "pushsync"
)

type PushSync struct {
	streamer      p2p.Streamer
	storer        storage.Storer
	peerSuggester topology.ClosestPeerer
	retryChunks   map[string]int
	retryChunksMu sync.Mutex
	quit          chan struct{}
	logger        logging.Logger
	metrics       metrics
}

type Options struct {
	Streamer      p2p.Streamer
	Storer        storage.Storer
	ClosestPeerer topology.ClosestPeerer
	Logger        logging.Logger
}

var (
	retryInterval        = 10 * time.Second // time interval between retries
	timeToWaitForReceipt = 2 * time.Second  // time to wait to get a receipt for a chunk
	MaxRetries           = 3                // No of time to retry sending the chunk
)

func New(o Options) *PushSync {
	ps := &PushSync{
		streamer:      o.Streamer,
		storer:        o.Storer,
		peerSuggester: o.ClosestPeerer,
		retryChunks:   make(map[string]int),
		logger:        o.Logger,
		metrics:       newMetrics(),
		quit:          make(chan struct{}),
	}

	ctx := context.Background()
	go ps.chunksWorker(ctx)

	return ps
}

func (s *PushSync) Protocol() p2p.ProtocolSpec {
	return p2p.ProtocolSpec{
		Name:    protocolName,
		Version: protocolVersion,
		StreamSpecs: []p2p.StreamSpec{
			{
				Name:    streamName,
				Handler: s.handler,
			},
		},
	}
}

func (ps *PushSync) Close() error {
	close(ps.quit)
	return nil
}

// handler handles chunk delivery from other node and inserts it in to the localstore,
//sends a receipt for the chunk. it also forwards the chunk to the closest peer.
func (ps *PushSync) handler(ctx context.Context, p p2p.Peer, stream p2p.Stream) error {

	w, r := protobuf.NewWriterAndReader(stream)
	defer stream.Close()

	var ch pb.Delivery
	if err := r.ReadMsg(&ch); err != nil {
		ps.metrics.ReceivedChunkErrorCounter.Inc()
		if err == io.EOF {
			return nil
		}
		return err
	}
	ps.metrics.ChunksSentCounter.Inc()

	// create chunk and store it in the local store
	addr := swarm.NewAddress(ch.Address)
	chunk := swarm.NewChunk(addr, ch.Data)
	_, err := ps.storer.Put(ctx, storage.ModePutSync, chunk)
	if err != nil {
		return err
	}

	// Send a receipt immediately once the storage of the chunk is successfull
	var receipt pb.Receipt
	if err := w.WriteMsg(receipt); err != nil {
		ps.metrics.SendReceiptErrorCounter.Inc()
		return err
	}
	ps.metrics.ReceiptsSentCounter.Inc()

	// Also push this chunk to the closest node too
	peer, err := ps.peerSuggester.ClosestPeer(addr)
	if err != nil {
		if errors.Is(err, topology.ErrWantSelf) {
			// i'm the closest - nothing to do
			return nil
		}
		return err
	}

	err = ps.storer.Set(ctx, storage.ModeSetSyncPush, chunk.Address())
	if err != nil {
		ps.logger.Errorf("pushsync: error setting chunks to synced", "addr", chunk.Address().String(), "err", err)
		ps.metrics.ErrorSettingChunkToSynced.Inc()
	}

	ps.metrics.TotalChunksSynced.Inc()

	return err
}

// chunksWorker is a loop that keepd looking for chunks that are locally uploaded ( by monitoring pushIndex )
// and pushes them to the closest peer.
func (ps *PushSync) chunksWorker(ctx context.Context) {
	var chunks <-chan swarm.Chunk
	var unsubscribe func()

	// timer, initially set to 0 to fall through select case on timer.C for initialisation
	timer := time.NewTimer(0)
	defer timer.Stop()
	chunksInBatch := -1

	for {
		select {
		// handle incoming chunks
		case ch, more := <-chunks:
			// if no more, set to nil, reset timer to 0 to finalise batch immediately
			if !more {
				chunks = nil
				var dur time.Duration
				if chunksInBatch == 0 {
					dur = 500 * time.Millisecond
				}
				timer.Reset(dur)
				break
			}

			chunksInBatch++
			ps.metrics.TotalChunksToBeSentCounter.Inc()
			peer, err := ps.peerSuggester.ClosestPeer(ch.Address())
			if err != nil {
				if errors.Is(err, topology.ErrWantSelf) {
					if err := ps.storer.Set(ctx, storage.ModeSetSyncPush, ch.Address()); err != nil {
						ps.logger.Error("pushsync: error setting chunks to synced", "err", err)
					}
					continue
				}
			}

			if err := ps.sendChunkAndReceiveReceipt(ctx, peer, ch); err != nil {
				ps.logger.Errorf("error while sending chunk or receiving receipt", "addr", ch.Address().String(), "err", err)
				continue
			}

			// set chunk status to synced, insert to db GC index
			if err := ps.storer.Set(ctx, storage.ModeSetSyncPush, ch.Address()); err != nil {
				ps.logger.Errorf("pushsync: error setting chunks to synced", "addr", ch.Address().String(), "err", err)
				ps.metrics.ErrorSettingChunkToSynced.Inc()
				continue
			}

			ps.metrics.TotalChunksSynced.Inc()

			// retry interval timer triggers starting from new
		case <-timer.C:
			// initially timer is set to go off as well as every time we hit the end of push index
			startTime := time.Now()

			// if subscribe was running, stop it
			if unsubscribe != nil {
				unsubscribe()
			}

			// and start iterating on Push index from the beginning
			chunks, unsubscribe = ps.storer.SubscribePush(ctx)

			// reset timer to go off after retryInterval
			timer.Reset(retryInterval)

			timeSpent := float64(time.Since(startTime))
			ps.metrics.MarkAndSweepTimer.Add(timeSpent)

		case <-ps.quit:
			if unsubscribe != nil {
				unsubscribe()
			}
			return
		}
	}
}

// sendChunkAndReceiveReceipt sends chunk to its destination
// by opening a stream to the closest peer. It then waits for
// an receipt from the peer.
func (ps *PushSync) sendChunkAndReceiveReceipt(ctx context.Context, peer swarm.Address, ch swarm.Chunk) error {
	startTimer := time.Now()

	streamer, err := ps.streamer.NewStream(ctx, peer, nil, protocolName, protocolVersion, streamName)
	if err != nil {
		return fmt.Errorf("new stream: %w", err)
	}
	defer streamer.Close()

	ps.retryChunksMu.Lock()

	var noOfRetries int
	var ok bool
	if noOfRetries, ok = ps.retryChunks[ch.Address().String()]; ok {
		if noOfRetries > MaxRetries {
			delete(ps.retryChunks, ch.Address().String())
			ps.metrics.RetriesExhaustedCounter.Inc()
			ps.retryChunksMu.Unlock()
			return fmt.Errorf("max retries exhausted ", "address", ch.Address().String())
		}
		noOfRetries++
	} else {
		ps.retryChunks[ch.Address().String()] = 0
	}
	ps.retryChunksMu.Unlock()

	w, r := protobuf.NewWriterAndReader(streamer)
	if err := w.WriteMsgWithTimeout(timeToWaitForReceipt, &pb.Delivery{
		Address: ch.Address().Bytes(),
		Data:    ch.Data(),
	}); err != nil {
		ps.metrics.SendChunkErrorCounter.Inc()
		return err
	}

	timeSpent := float64(time.Since(startTimer))
	ps.metrics.SendChunkTimer.Add(timeSpent)
	ps.metrics.ChunksSentCounter.Inc()

	startTimer = time.Now()
	var receipt pb.Receipt
	if err := r.ReadMsg(&receipt); err != nil {
		ps.metrics.ReceiveReceiptErrorCounter.Inc()
		return err
	}
	timeSpent = float64(time.Since(startTimer))
	ps.metrics.ReceiptRTT.Add(timeSpent)
	ps.metrics.ReceiptsReceivedCounter.Inc()

	return nil
}
