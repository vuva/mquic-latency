package flowcontrol

import (
	"errors"
	"time"

	"github.com/lucas-clemente/quic-go/congestion"
	"github.com/lucas-clemente/quic-go/internal/handshake"
	"github.com/lucas-clemente/quic-go/internal/protocol"
	"github.com/lucas-clemente/quic-go/internal/utils"
)

type flowController struct {
	streamID                protocol.StreamID
	contributesToConnection bool // does the stream contribute to connection level flow control

	connectionParameters handshake.ConnectionParametersManager
	rttStats             *congestion.RTTStats
	remoteRTTs           map[protocol.PathID]time.Duration

	bytesSent  protocol.ByteCount
	sendWindow protocol.ByteCount

	// For statistics purpose
	bytesRetrans protocol.ByteCount

	lastWindowUpdateTime time.Time

	bytesRead                 protocol.ByteCount
	highestReceived           protocol.ByteCount
	receiveWindow             protocol.ByteCount
	receiveWindowIncrement    protocol.ByteCount
	maxReceiveWindowIncrement protocol.ByteCount
}

// ErrReceivedSmallerByteOffset occurs if the ByteOffset received is smaller than a ByteOffset that was set previously
var ErrReceivedSmallerByteOffset = errors.New("Received a smaller byte offset")

// newFlowController gets a new flow controller
func newFlowController(streamID protocol.StreamID, contributesToConnection bool, connectionParameters handshake.ConnectionParametersManager, rttStats *congestion.RTTStats, remoteRTTs map[protocol.PathID]time.Duration) *flowController {
	fc := flowController{
		streamID:                streamID,
		contributesToConnection: contributesToConnection,
		connectionParameters:    connectionParameters,
		rttStats:                rttStats,
		remoteRTTs:              remoteRTTs,
	}

	if streamID == 0 {
		fc.receiveWindow = connectionParameters.GetReceiveConnectionFlowControlWindow()
		fc.receiveWindowIncrement = fc.receiveWindow
		fc.maxReceiveWindowIncrement = connectionParameters.GetMaxReceiveConnectionFlowControlWindow()
	} else {
		fc.receiveWindow = connectionParameters.GetReceiveStreamFlowControlWindow()
		fc.receiveWindowIncrement = fc.receiveWindow
		fc.maxReceiveWindowIncrement = connectionParameters.GetMaxReceiveStreamFlowControlWindow()
	}

	return &fc
}

func (c *flowController) ContributesToConnection() bool {
	return c.contributesToConnection
}

func (c *flowController) getSendWindow() protocol.ByteCount {
	if c.sendWindow == 0 {
		if c.streamID == 0 {
			return c.connectionParameters.GetSendConnectionFlowControlWindow()
		}
		return c.connectionParameters.GetSendStreamFlowControlWindow()
	}
	return c.sendWindow
}

func (c *flowController) AddBytesSent(n protocol.ByteCount) {
	c.bytesSent += n
}

func (c *flowController) GetBytesSent() protocol.ByteCount {
	return c.bytesSent
}

func (c *flowController) AddBytesRetrans(n protocol.ByteCount) {
	c.bytesRetrans += n
}

func (c *flowController) GetBytesRetrans() protocol.ByteCount {
	return c.bytesRetrans
}

// UpdateSendWindow should be called after receiving a WindowUpdateFrame
// it returns true if the window was actually updated
func (c *flowController) UpdateSendWindow(newOffset protocol.ByteCount) bool {
	if newOffset > c.sendWindow {
		c.sendWindow = newOffset

		if c.streamID > 3 {
			utils.Debugf("\n on Stream %d UpdateSendWindow() c.sendWindow %d newOffset %d at %d", c.streamID, c.sendWindow, newOffset, time.Now().UnixNano())
		}
		return true
	}
	return false
}

func (c *flowController) SendWindowSize() protocol.ByteCount {
	sendWindow := c.getSendWindow()
	if c.streamID > 3 {
		utils.Debugf("\n on Stream %d SendWindowSize() %d sendWindow %d", c.streamID, sendWindow-c.bytesSent, sendWindow)
	}
	if c.bytesSent > sendWindow { // should never happen, but make sure we don't do an underflow here
		return 0
	}
	return sendWindow - c.bytesSent
}

func (c *flowController) SendWindowOffset() protocol.ByteCount {
	return c.getSendWindow()
}

// UpdateHighestReceived updates the highestReceived value, if the byteOffset is higher
// Should **only** be used for the stream-level FlowController
// it returns an ErrReceivedSmallerByteOffset if the received byteOffset is smaller than any byteOffset received before
// This error occurs every time StreamFrames get reordered and has to be ignored in that case
// It should only be treated as an error when resetting a stream
func (c *flowController) UpdateHighestReceived(byteOffset protocol.ByteCount) (protocol.ByteCount, error) {
	if byteOffset == c.highestReceived {
		return 0, nil
	}
	if byteOffset > c.highestReceived {
		increment := byteOffset - c.highestReceived
		c.highestReceived = byteOffset
		return increment, nil
	}
	return 0, ErrReceivedSmallerByteOffset
}

// IncrementHighestReceived adds an increment to the highestReceived value
// Should **only** be used for the connection-level FlowController
func (c *flowController) IncrementHighestReceived(increment protocol.ByteCount) {
	c.highestReceived += increment
}

func (c *flowController) AddBytesRead(n protocol.ByteCount) {
	// pretend we sent a WindowUpdate when reading the first byte
	// this way auto-tuning of the window increment already works for the first WindowUpdate
	if c.bytesRead == 0 {
		c.lastWindowUpdateTime = time.Now()
	}
	c.bytesRead += n
}

// MaybeUpdateWindow updates the receive window, if necessary
// if the receive window increment is changed, the new value is returned, otherwise a 0
// the last return value is the new offset of the receive window
func (c *flowController) MaybeUpdateWindow() (bool, protocol.ByteCount /* new increment */, protocol.ByteCount /* new offset */) {
	diff := c.receiveWindow - c.bytesRead
	if c.streamID > 3 {
		utils.Debugf("\n MaybeUpdateWindow diff %d c.receiveWindow %d c.bytesRead %d", diff, c.receiveWindow, c.bytesRead)

	}
	// Chromium implements the same threshold
	if diff < (c.receiveWindowIncrement / 2) {
		var newWindowIncrement protocol.ByteCount
		oldWindowIncrement := c.receiveWindowIncrement

		c.maybeAdjustWindowIncrement()
		if c.receiveWindowIncrement != oldWindowIncrement {
			newWindowIncrement = c.receiveWindowIncrement
		}

		c.lastWindowUpdateTime = time.Now()
		c.receiveWindow = c.bytesRead + c.receiveWindowIncrement
		utils.Debugf("\n UpdateWindow diff %d newreceiveWindow %d receiveWindowIncrement %d newreceiveWindowIncrement at %d", diff, c.receiveWindow, c.receiveWindowIncrement, newWindowIncrement, time.Now().UnixNano())
		return true, newWindowIncrement, c.receiveWindow
	}

	return false, c.receiveWindowIncrement, c.receiveWindow
}

// maybeAdjustWindowIncrement increases the receiveWindowIncrement if we're sending WindowUpdates too often
func (c *flowController) maybeAdjustWindowIncrement() {
	if c.lastWindowUpdateTime.IsZero() {
		return
	}

	rtt := c.rttStats.SmoothedRTT()
	if rtt == 0 {
		return
	}

	timeSinceLastWindowUpdate := time.Since(c.lastWindowUpdateTime)

	var maxRemoteRTT time.Duration
	for _, remoteRTT := range c.remoteRTTs {
		maxRemoteRTT = utils.MaxDuration(maxRemoteRTT, remoteRTT)
	}
	utils.Debugf("Check timeSinceLastWindowUpdate stream %d %d %d %d", c.streamID, timeSinceLastWindowUpdate, rtt, maxRemoteRTT)
	// interval between the window updates is sufficiently large, no need to increase the increment
	if timeSinceLastWindowUpdate >= 2*utils.MaxDuration(rtt, maxRemoteRTT) {
		return
	}

	oldWindowSize := c.receiveWindowIncrement
	c.receiveWindowIncrement = utils.MinByteCount(2*c.receiveWindowIncrement, c.maxReceiveWindowIncrement)

	// debug log, if the window size was actually increased
	if oldWindowSize < c.receiveWindowIncrement {
		newWindowSize := c.receiveWindowIncrement / (1 << 10)
		if c.streamID == 0 {
			utils.Debugf("Increasing receive flow control window for the connection to %d kB", newWindowSize)
		} else {
			utils.Debugf("Increasing receive flow control window increment for stream %d to %d kB", c.streamID, newWindowSize)
		}
	}
}

// EnsureMinimumWindowIncrement sets a minimum window increment
// it is intended be used for the connection-level flow controller
// it should make sure that the connection-level window is increased when a stream-level window grows
func (c *flowController) EnsureMinimumWindowIncrement(inc protocol.ByteCount) {
	if inc > c.receiveWindowIncrement {
		c.receiveWindowIncrement = utils.MinByteCount(inc, c.maxReceiveWindowIncrement)
		c.lastWindowUpdateTime = time.Time{} // disables autotuning for the next window update
	}
}

func (c *flowController) CheckFlowControlViolation() bool {
	return c.highestReceived > c.receiveWindow
}
