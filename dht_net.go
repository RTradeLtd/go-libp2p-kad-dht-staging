package dht

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-core/helpers"
	"github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"

	"github.com/libp2p/go-libp2p-kad-dht/metrics"
	pb "github.com/libp2p/go-libp2p-kad-dht/pb"

	ggio "github.com/gogo/protobuf/io"

	"github.com/libp2p/go-msgio"
	"go.opencensus.io/stats"
	"go.opencensus.io/tag"
)

var dhtReadMessageTimeout = 10 * time.Second
var dhtStreamIdleTimeout = 1 * time.Minute

// ErrReadTimeout is an error that occurs when no message is read within the timeout period.
var ErrReadTimeout = fmt.Errorf("timed out reading response")

// The Protobuf writer performs multiple small writes when writing a message.
// We need to buffer those writes, to make sure that we're not sending a new
// packet for every single write.
type bufferedDelimitedWriter struct {
	*bufio.Writer
	ggio.WriteCloser
}

var writerPool = sync.Pool{
	New: func() interface{} {
		w := bufio.NewWriter(nil)
		return &bufferedDelimitedWriter{
			Writer:      w,
			WriteCloser: ggio.NewDelimitedWriter(w),
		}
	},
}

func writeMsg(w io.Writer, mes *pb.Message) error {
	bw := writerPool.Get().(*bufferedDelimitedWriter)
	bw.Reset(w)
	err := bw.WriteMsg(mes)
	if err == nil {
		err = bw.Flush()
	}
	bw.Reset(nil)
	writerPool.Put(bw)
	return err
}

func (w *bufferedDelimitedWriter) Flush() error {
	return w.Writer.Flush()
}

// handleNewStream implements the network.StreamHandler
func (dht *IpfsDHT) handleNewStream(s network.Stream) {
	defer s.Reset() //nolint
	if dht.handleNewMessage(s) {
		// Gracefully close the stream for writes.
		s.Close()
	}
}

// Returns true on orderly completion of writes (so we can Close the stream).
func (dht *IpfsDHT) handleNewMessage(s network.Stream) bool {
	ctx := dht.ctx
	r := msgio.NewVarintReaderSize(s, network.MessageSizeMax)

	mPeer := s.Conn().RemotePeer()

	timer := time.AfterFunc(dhtStreamIdleTimeout, func() { _ = s.Reset() })
	defer timer.Stop()

	for {
		if dht.getMode() != modeServer {
			logger.Errorf("ignoring incoming dht message while not in server mode")
			return false
		}

		var req pb.Message
		msgbytes, err := r.ReadMsg()
		msgLen := len(msgbytes)
		if err != nil {
			r.ReleaseMsg(msgbytes)
			if err == io.EOF {
				return true
			}
			// This string test is necessary because there isn't a single stream reset error
			// instance	in use.
			if err.Error() != "stream reset" {
				logger.Debugf("error reading message: %#v", err)
			}
			if msgLen > 0 {
				_ = stats.RecordWithTags(ctx,
					[]tag.Mutator{tag.Upsert(metrics.KeyMessageType, "UNKNOWN")},
					metrics.ReceivedMessages.M(1),
					metrics.ReceivedMessageErrors.M(1),
					metrics.ReceivedBytes.M(int64(msgLen)),
				)
			}
			return false
		}
		err = req.Unmarshal(msgbytes)
		r.ReleaseMsg(msgbytes)
		if err != nil {
			logger.Debugf("error unmarshaling message: %#v", err)
			_ = stats.RecordWithTags(ctx,
				[]tag.Mutator{tag.Upsert(metrics.KeyMessageType, "UNKNOWN")},
				metrics.ReceivedMessages.M(1),
				metrics.ReceivedMessageErrors.M(1),
				metrics.ReceivedBytes.M(int64(msgLen)),
			)
			return false
		}

		timer.Reset(dhtStreamIdleTimeout)

		startTime := time.Now()
		ctx, _ := tag.New(ctx,
			tag.Upsert(metrics.KeyMessageType, req.GetType().String()),
		)

		stats.Record(ctx,
			metrics.ReceivedMessages.M(1),
			metrics.ReceivedBytes.M(int64(msgLen)),
		)

		handler := dht.handlerForMsgType(req.GetType())
		if handler == nil {
			stats.Record(ctx, metrics.ReceivedMessageErrors.M(1))
			logger.Warnw("can't handle received message", "from", mPeer, "type", req.GetType())
			return false
		}

		// a peer has queried us, let's add it to RT
		dht.peerFound(dht.ctx, mPeer, true)

		logger.Debugw("handling message",
			"type", req.GetType(),
			"key", req.GetKey(),
			"from", mPeer,
		)
		resp, err := handler(ctx, mPeer, &req)
		if err != nil {
			stats.Record(ctx, metrics.ReceivedMessageErrors.M(1))
			logger.Debugw(
				"error handling message",
				"type", req.GetType(),
				"key", req.GetKey(),
				"from", mPeer,
				"error", err)
			return false
		}

		logger.Debugw(
			"handled message",
			"type", req.GetType(),
			"key", req.GetKey(),
			"from", mPeer,
			"time", time.Since(startTime),
		)

		if resp == nil {
			continue
		}

		// send out response msg
		err = writeMsg(s, resp)
		if err != nil {
			stats.Record(ctx, metrics.ReceivedMessageErrors.M(1))
			logger.Debugw(
				"error writing response",
				"type", req.GetType(),
				"key", req.GetKey(),
				"from", mPeer,
				"error", err)
			return false
		}

		elapsedTime := time.Since(startTime)

		logger.Debugw(
			"responded to message",
			"type", req.GetType(),
			"key", req.GetKey(),
			"from", mPeer,
			"time", elapsedTime,
		)

		latencyMillis := float64(elapsedTime) / float64(time.Millisecond)
		stats.Record(ctx, metrics.InboundRequestLatency.M(latencyMillis))
	}
}

// sendRequest sends out a request, but also makes sure to
// measure the RTT for latency measurements.
func (dht *IpfsDHT) sendRequest(ctx context.Context, p peer.ID, pmes *pb.Message) (*pb.Message, error) {
	ctx, _ = tag.New(ctx, metrics.UpsertMessageType(pmes))

	ms, err := dht.messageSenderForPeer(ctx, p)
	if err != nil {
		stats.Record(ctx,
			metrics.SentRequests.M(1),
			metrics.SentRequestErrors.M(1),
		)
		logger.Debugw("request failed to open message sender", "error", err, "to", p)
		return nil, err
	}

	start := time.Now()

	rpmes, err := ms.SendRequest(ctx, pmes)
	if err != nil {
		stats.Record(ctx,
			metrics.SentRequests.M(1),
			metrics.SentRequestErrors.M(1),
		)
		logger.Debugw("request failed", "error", err, "to", p)
		return nil, err
	}

	stats.Record(ctx,
		metrics.SentRequests.M(1),
		metrics.SentBytes.M(int64(pmes.Size())),
		metrics.OutboundRequestLatency.M(float64(time.Since(start))/float64(time.Millisecond)),
	)
	dht.peerstore.RecordLatency(p, time.Since(start))
	return rpmes, nil
}

// sendMessage sends out a message
func (dht *IpfsDHT) sendMessage(ctx context.Context, p peer.ID, pmes *pb.Message) error {
	ctx, _ = tag.New(ctx, metrics.UpsertMessageType(pmes))

	ms, err := dht.messageSenderForPeer(ctx, p)
	if err != nil {
		stats.Record(ctx,
			metrics.SentMessages.M(1),
			metrics.SentMessageErrors.M(1),
		)
		logger.Debugw("message failed to open message sender", "error", err, "to", p)
		return err
	}

	if err := ms.SendMessage(ctx, pmes); err != nil {
		stats.Record(ctx,
			metrics.SentMessages.M(1),
			metrics.SentMessageErrors.M(1),
		)
		logger.Debugw("message failed", "error", err, "to", p)
		return err
	}

	stats.Record(ctx,
		metrics.SentMessages.M(1),
		metrics.SentBytes.M(int64(pmes.Size())),
	)
	return nil
}

func (dht *IpfsDHT) messageSenderForPeer(ctx context.Context, p peer.ID) (*messageSender, error) {
	dht.smlk.Lock()
	ms, ok := dht.strmap[p]
	if ok {
		dht.smlk.Unlock()
		return ms, nil
	}
	ms = &messageSender{p: p, dht: dht, lk: newCtxMutex()}
	dht.strmap[p] = ms
	dht.smlk.Unlock()

	if err := ms.prepOrInvalidate(ctx); err != nil {
		dht.smlk.Lock()
		defer dht.smlk.Unlock()

		if msCur, ok := dht.strmap[p]; ok {
			// Changed. Use the new one, old one is invalid and
			// not in the map so we can just throw it away.
			if ms != msCur {
				return msCur, nil
			}
			// Not changed, remove the now invalid stream from the
			// map.
			delete(dht.strmap, p)
		}
		// Invalid but not in map. Must have been removed by a disconnect.
		return nil, err
	}
	// All ready to go.
	return ms, nil
}

type messageSender struct {
	s   network.Stream
	r   msgio.ReadCloser
	lk  ctxMutex
	p   peer.ID
	dht *IpfsDHT

	invalid   bool
	singleMes int
}

// invalidate is called before this messageSender is removed from the strmap.
// It prevents the messageSender from being reused/reinitialized and then
// forgotten (leaving the stream open).
func (ms *messageSender) invalidate() {
	ms.invalid = true
	if ms.s != nil {
		_ = ms.s.Reset()
		ms.s = nil
	}
}

func (ms *messageSender) prepOrInvalidate(ctx context.Context) error {
	if err := ms.lk.Lock(ctx); err != nil {
		return err
	}
	defer ms.lk.Unlock()

	if err := ms.prep(ctx); err != nil {
		ms.invalidate()
		return err
	}
	return nil
}

func (ms *messageSender) prep(ctx context.Context) error {
	if ms.invalid {
		return fmt.Errorf("message sender has been invalidated")
	}
	if ms.s != nil {
		return nil
	}

	// We only want to speak to peers using our primary protocols. We do not want to query any peer that only speaks
	// one of the secondary "server" protocols that we happen to support (e.g. older nodes that we can respond to for
	// backwards compatibility reasons).
	nstr, err := ms.dht.host.NewStream(ctx, ms.p, ms.dht.protocols...)
	if err != nil {
		return err
	}

	ms.r = msgio.NewVarintReaderSize(nstr, network.MessageSizeMax)
	ms.s = nstr

	return nil
}

// streamReuseTries is the number of times we will try to reuse a stream to a
// given peer before giving up and reverting to the old one-message-per-stream
// behaviour.
const streamReuseTries = 3

func (ms *messageSender) SendMessage(ctx context.Context, pmes *pb.Message) error {
	if err := ms.lk.Lock(ctx); err != nil {
		return err
	}
	defer ms.lk.Unlock()

	retry := false
	for {
		if err := ms.prep(ctx); err != nil {
			return err
		}

		if err := ms.writeMsg(pmes); err != nil {
			_ = ms.s.Reset()
			ms.s = nil

			if retry {
				logger.Debugw("error writing message", "error", err)
				return err
			}
			logger.Debugw("error writing message", "error", err, "retrying", true)
			retry = true
			continue
		}

		if ms.singleMes > streamReuseTries {
			go helpers.FullClose(ms.s)
			ms.s = nil
		} else if retry {
			ms.singleMes++
		}

		return nil
	}
}

func (ms *messageSender) SendRequest(ctx context.Context, pmes *pb.Message) (*pb.Message, error) {
	if err := ms.lk.Lock(ctx); err != nil {
		return nil, err
	}
	defer ms.lk.Unlock()

	retry := false
	for {
		if err := ms.prep(ctx); err != nil {
			return nil, err
		}

		if err := ms.writeMsg(pmes); err != nil {
			_ = ms.s.Reset()
			ms.s = nil

			if retry {
				logger.Debugw("error writing message", "error", err)
				return nil, err
			}
			logger.Debugw("error writing message", "error", err, "retrying", true)
			retry = true
			continue
		}

		mes := new(pb.Message)
		if err := ms.ctxReadMsg(ctx, mes); err != nil {
			_ = ms.s.Reset()
			ms.s = nil

			if retry {
				logger.Debugw("error reading message", "error", err)
				return nil, err
			}
			logger.Debugw("error reading message", "error", err, "retrying", true)
			retry = true
			continue
		}

		if ms.singleMes > streamReuseTries {
			go helpers.FullClose(ms.s)
			ms.s = nil
		} else if retry {
			ms.singleMes++
		}

		return mes, nil
	}
}

func (ms *messageSender) writeMsg(pmes *pb.Message) error {
	return writeMsg(ms.s, pmes)
}

func (ms *messageSender) ctxReadMsg(ctx context.Context, mes *pb.Message) error {
	errc := make(chan error, 1)
	go func(r msgio.ReadCloser) {
		bytes, err := r.ReadMsg()
		defer r.ReleaseMsg(bytes)
		if err != nil {
			errc <- err
			return
		}
		errc <- mes.Unmarshal(bytes)
	}(ms.r)

	t := time.NewTimer(dhtReadMessageTimeout)
	defer t.Stop()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return ErrReadTimeout
	}
}
