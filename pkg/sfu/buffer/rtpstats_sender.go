// Copyright 2023 LiveKit, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package buffer

import (
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/pion/rtcp"
	"go.uber.org/zap/zapcore"

	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
)

const (
	cSnInfoSize = 4096
	cSnInfoMask = cSnInfoSize - 1

	cSenderReportInitialWait = time.Second
)

// -------------------------------------------------------------------

type snInfoFlag byte

const (
	snInfoFlagMarker snInfoFlag = 1 << iota
	snInfoFlagPadding
	snInfoFlagOutOfOrder
)

type snInfo struct {
	pktSize uint16
	hdrSize uint8
	flags   snInfoFlag
}

// -------------------------------------------------------------------

type intervalStats struct {
	packets            uint64
	bytes              uint64
	headerBytes        uint64
	packetsPadding     uint64
	bytesPadding       uint64
	headerBytesPadding uint64
	packetsLost        uint64
	packetsOutOfOrder  uint64
	frames             uint32
	packetsNotFound    uint64
}

func (is *intervalStats) aggregate(other *intervalStats) {
	if is == nil || other == nil {
		return
	}

	is.packets += other.packets
	is.bytes += other.bytes
	is.headerBytes += other.headerBytes
	is.packetsPadding += other.packetsPadding
	is.bytesPadding += other.bytesPadding
	is.headerBytesPadding += other.headerBytesPadding
	is.packetsLost += other.packetsLost
	is.packetsOutOfOrder += other.packetsOutOfOrder
	is.frames += other.frames
	is.packetsNotFound += other.packetsNotFound
}

func (is *intervalStats) ToString() string {
	if is == nil {
		return "-"
	}

	return fmt.Sprintf("p: %d, b: %d, hb: %d, pp: %d, bp: %d, hbp: %d, pl: %d, pooo: %d, f: %d, pnf: %d",
		is.packets,
		is.bytes,
		is.headerBytes,
		is.packetsPadding,
		is.bytesPadding,
		is.headerBytesPadding,
		is.packetsLost,
		is.packetsOutOfOrder,
		is.frames,
		is.packetsNotFound,
	)
}

// -------------------------------------------------------------------

type senderSnapshot struct {
	isValid bool

	startTime time.Time

	extStartSN  uint64
	bytes       uint64
	headerBytes uint64

	packetsPadding     uint64
	bytesPadding       uint64
	headerBytesPadding uint64

	packetsDuplicate     uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64

	packetsOutOfOrder uint64

	packetsLostFeed uint64
	packetsLost     uint64

	frames uint32

	nacks uint32
	plis  uint32
	firs  uint32

	maxRtt        uint32
	maxJitterFeed float64
	maxJitter     float64

	extLastRRSN   uint64
	intervalStats intervalStats
}

type RTPStatsSender struct {
	*rtpStatsBase

	extStartSN         uint64
	extHighestSN       uint64
	extHighestSNFromRR uint64

	lastRRTime time.Time
	lastRR     rtcp.ReceptionReport

	extStartTS   uint64
	extHighestTS uint64

	packetsLostFromRR uint64

	jitterFromRR    float64
	maxJitterFromRR float64

	snInfos [cSnInfoSize]snInfo

	nextSenderSnapshotID uint32
	senderSnapshots      []senderSnapshot

	clockSkewCount             int
	metadataCacheOverflowCount int
	largeJumpNegativeCount     int
	largeJumpCount             int
	timeReversedCount          int
}

func NewRTPStatsSender(params RTPStatsParams) *RTPStatsSender {
	return &RTPStatsSender{
		rtpStatsBase:         newRTPStatsBase(params),
		nextSenderSnapshotID: cFirstSnapshotID,
		senderSnapshots:      make([]senderSnapshot, 2),
	}
}

func (r *RTPStatsSender) Seed(from *RTPStatsSender) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.seed(from.rtpStatsBase) {
		return
	}

	r.extStartSN = from.extStartSN
	r.extHighestSN = from.extHighestSN
	r.extHighestSNFromRR = from.extHighestSNFromRR

	r.lastRRTime = from.lastRRTime
	r.lastRR = from.lastRR

	r.extStartTS = from.extStartTS
	r.extHighestTS = from.extHighestTS

	r.packetsLostFromRR = from.packetsLostFromRR

	r.jitterFromRR = from.jitterFromRR
	r.maxJitterFromRR = from.maxJitterFromRR

	r.snInfos = from.snInfos

	r.nextSenderSnapshotID = from.nextSenderSnapshotID
	r.senderSnapshots = make([]senderSnapshot, cap(from.senderSnapshots))
	copy(r.senderSnapshots, from.senderSnapshots)
}

func (r *RTPStatsSender) NewSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.newSnapshotID(r.extHighestSN)
}

func (r *RTPStatsSender) NewSenderSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	id := r.nextSenderSnapshotID
	r.nextSenderSnapshotID++

	if cap(r.senderSnapshots) < int(r.nextSenderSnapshotID-cFirstSnapshotID) {
		senderSnapshots := make([]senderSnapshot, r.nextSenderSnapshotID-cFirstSnapshotID)
		copy(senderSnapshots, r.senderSnapshots)
		r.senderSnapshots = senderSnapshots
	}

	if r.initialized {
		r.senderSnapshots[id-cFirstSnapshotID] = r.initSenderSnapshot(time.Now(), r.extHighestSN)
	}
	return id
}

func (r *RTPStatsSender) Update(
	packetTime int64,
	extSequenceNumber uint64,
	extTimestamp uint64,
	marker bool,
	hdrSize int,
	payloadSize int,
	paddingSize int,
) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	if !r.initialized {
		if payloadSize == 0 {
			// do not start on a padding only packet
			return
		}

		r.initialized = true

		r.startTime = time.Now()

		r.firstTime = packetTime
		r.highestTime = packetTime

		r.extStartSN = extSequenceNumber
		r.extHighestSN = extSequenceNumber - 1

		r.extStartTS = extTimestamp
		r.extHighestTS = extTimestamp

		// initialize snapshots if any
		for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
			r.snapshots[i] = r.initSnapshot(r.startTime, r.extStartSN)
		}
		for i := uint32(0); i < r.nextSenderSnapshotID-cFirstSnapshotID; i++ {
			r.senderSnapshots[i] = r.initSenderSnapshot(r.startTime, r.extStartSN)
		}

		r.logger.Debugw(
			"rtp sender stream start",
			"rtpStats", lockedRTPStatsSenderLogEncoder{r},
		)
	}

	pktSize := uint64(hdrSize + payloadSize + paddingSize)
	isDuplicate := false
	gapSN := int64(extSequenceNumber - r.extHighestSN)
	getLoggingFields := func() []interface{} {
		return []interface{}{
			"currSN", extSequenceNumber,
			"gapSN", gapSN,
			"currTS", extTimestamp,
			"gapTS", int64(extTimestamp - r.extHighestTS),
			"packetTime", packetTime,
			"marker", marker,
			"hdrSize", hdrSize,
			"payloadSize", payloadSize,
			"paddingSize", paddingSize,
			"rtpStats", lockedRTPStatsSenderLogEncoder{r},
		}
	}
	if gapSN <= 0 { // duplicate OR out-of-order
		if payloadSize == 0 && extSequenceNumber < r.extStartSN {
			// do not start on a padding only packet
			return
		}

		if extSequenceNumber < r.extStartSN {
			r.packetsLost += r.extStartSN - extSequenceNumber

			// adjust start of snapshots
			for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
				s := &r.snapshots[i]
				if s.extStartSN == r.extStartSN {
					s.extStartSN = extSequenceNumber
				}
			}
			for i := uint32(0); i < r.nextSenderSnapshotID-cFirstSnapshotID; i++ {
				s := &r.senderSnapshots[i]
				if s.extStartSN == r.extStartSN {
					s.extStartSN = extSequenceNumber
					if s.extLastRRSN == (r.extStartSN - 1) {
						s.extLastRRSN = extSequenceNumber - 1
					}
				}
			}

			r.logger.Infow(
				"adjusting start sequence number",
				append(getLoggingFields(),
					"snAfter", extSequenceNumber,
					"tsAfter", extTimestamp,
				)...,
			)
			r.extStartSN = extSequenceNumber
		}

		if gapSN != 0 {
			r.packetsOutOfOrder++
		}

		if !r.isSnInfoLost(extSequenceNumber, r.extHighestSN) {
			r.bytesDuplicate += pktSize
			r.headerBytesDuplicate += uint64(hdrSize)
			r.packetsDuplicate++
			isDuplicate = true
		} else {
			r.packetsLost--
			r.setSnInfo(extSequenceNumber, r.extHighestSN, uint16(pktSize), uint8(hdrSize), uint16(payloadSize), marker, true)
		}

		if !isDuplicate && -gapSN >= cSequenceNumberLargeJumpThreshold {
			r.largeJumpNegativeCount++
			if (r.largeJumpNegativeCount-1)%100 == 0 {
				r.logger.Warnw(
					"large sequence number gap negative", nil,
					append(getLoggingFields(), "count", r.largeJumpNegativeCount)...,
				)
			}
		}
	} else { // in-order
		if gapSN >= cSequenceNumberLargeJumpThreshold {
			r.largeJumpCount++
			if (r.largeJumpCount-1)%100 == 0 {
				r.logger.Warnw(
					"large sequence number gap", nil,
					append(getLoggingFields(), "count", r.largeJumpCount)...,
				)
			}
		}

		if extTimestamp < r.extHighestTS {
			r.timeReversedCount++
			if (r.timeReversedCount-1)%100 == 0 {
				r.logger.Warnw(
					"time reversed", nil,
					append(getLoggingFields(), "count", r.timeReversedCount)...,
				)
			}
		}

		// update gap histogram
		r.updateGapHistogram(int(gapSN))

		// update missing sequence numbers
		r.clearSnInfos(r.extHighestSN+1, extSequenceNumber)
		r.packetsLost += uint64(gapSN - 1)

		r.setSnInfo(extSequenceNumber, r.extHighestSN, uint16(pktSize), uint8(hdrSize), uint16(payloadSize), marker, false)

		r.extHighestSN = extSequenceNumber
	}

	if extTimestamp < r.extStartTS {
		r.logger.Infow(
			"adjusting start timestamp",
			append(getLoggingFields(),
				"snAfter", extSequenceNumber,
				"tsAfter", extTimestamp,
			)...,
		)
		r.extStartTS = extTimestamp
	}

	if extTimestamp > r.extHighestTS {
		// update only on first packet as same timestamp could be in multiple packets.
		// NOTE: this may not be the first packet with this time stamp if there is packet loss.
		if payloadSize > 0 {
			// skip updating on padding only packets as they could re-use an old timestamp
			r.highestTime = packetTime
		}
		r.extHighestTS = extTimestamp
	}

	if !isDuplicate {
		if payloadSize == 0 {
			r.packetsPadding++
			r.bytesPadding += pktSize
			r.headerBytesPadding += uint64(hdrSize)
		} else {
			r.bytes += pktSize
			r.headerBytes += uint64(hdrSize)

			if marker {
				r.frames++
			}

			jitter := r.updateJitter(extTimestamp, packetTime)
			for i := uint32(0); i < r.nextSenderSnapshotID-cFirstSnapshotID; i++ {
				s := &r.senderSnapshots[i]
				if jitter > s.maxJitterFeed {
					s.maxJitterFeed = jitter
				}
			}
		}
	}
}

func (r *RTPStatsSender) GetTotalPacketsPrimary() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.getTotalPacketsPrimary(r.extStartSN, r.extHighestSN)
}

func (r *RTPStatsSender) UpdateFromReceiverReport(rr rtcp.ReceptionReport) (rtt uint32, isRttChanged bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.initialized || !r.endTime.IsZero() {
		return
	}

	extHighestSNFromRR := r.extHighestSNFromRR&0xFFFF_FFFF_0000_0000 + uint64(rr.LastSequenceNumber)
	if !r.lastRRTime.IsZero() {
		if (rr.LastSequenceNumber-r.lastRR.LastSequenceNumber) < (1<<31) && rr.LastSequenceNumber < r.lastRR.LastSequenceNumber {
			extHighestSNFromRR += (1 << 32)
		}
	}
	if (extHighestSNFromRR + (r.extStartSN & 0xFFFF_FFFF_FFFF_0000)) < r.extStartSN {
		// it is possible that the `LastSequenceNumber` in the receiver report is before the starting
		// sequence number when dummy packets are used to trigger Pion's OnTrack path.
		return
	}

	if !r.lastRRTime.IsZero() && r.extHighestSNFromRR > extHighestSNFromRR {
		r.logger.Debugw(
			fmt.Sprintf("receiver report potentially out of order, highestSN: existing: %d, received: %d", r.extHighestSNFromRR, extHighestSNFromRR),
			"sinceLastRR", time.Since(r.lastRRTime).String(),
			"receivedRR", rr,
			"rtpStats", lockedRTPStatsSenderLogEncoder{r},
		)
		return
	}

	r.extHighestSNFromRR = extHighestSNFromRR

	if r.srNewest != nil {
		var err error
		rtt, err = mediatransportutil.GetRttMs(&rr, r.srNewest.NTPTimestamp, r.srNewest.At)
		if err == nil {
			isRttChanged = rtt != r.rtt
		} else {
			r.logger.Debugw("error getting rtt", "error", err)
		}
	}

	// This is 24-bit max in the protocol. So, technically doesn't need extended type. But, done for consistency.
	packetsLostFromRR := r.packetsLostFromRR&0xFFFF_FFFF_0000_0000 + uint64(rr.TotalLost)
	if (rr.TotalLost-r.lastRR.TotalLost) < (1<<31) && rr.TotalLost < r.lastRR.TotalLost {
		packetsLostFromRR += (1 << 32)
	}
	r.packetsLostFromRR = packetsLostFromRR

	if isRttChanged {
		r.rtt = rtt
		if rtt > r.maxRtt {
			r.maxRtt = rtt
		}
	}

	r.jitterFromRR = float64(rr.Jitter)
	if r.jitterFromRR > r.maxJitterFromRR {
		r.maxJitterFromRR = r.jitterFromRR
	}

	// update snapshots
	for i := uint32(0); i < r.nextSnapshotID-cFirstSnapshotID; i++ {
		s := &r.snapshots[i]
		if isRttChanged && rtt > s.maxRtt {
			s.maxRtt = rtt
		}
	}

	extReceivedRRSN := r.extHighestSNFromRR + (r.extStartSN & 0xFFFF_FFFF_FFFF_0000)
	for i := uint32(0); i < r.nextSenderSnapshotID-cFirstSnapshotID; i++ {
		s := &r.senderSnapshots[i]
		if isRttChanged && rtt > s.maxRtt {
			s.maxRtt = rtt
		}

		if r.jitterFromRR > s.maxJitter {
			s.maxJitter = r.jitterFromRR
		}

		if int64(extReceivedRRSN-s.extLastRRSN) < 0 || (extReceivedRRSN-s.extLastRRSN) > (1<<15) {
			timeSinceLastRR := time.Since(r.lastRRTime)
			if r.lastRRTime.IsZero() {
				timeSinceLastRR = time.Since(r.startTime)
			}
			r.logger.Infow(
				"rr interval too big, skipping",
				"timeSinceLastRR", timeSinceLastRR.String(),
				"receivedRR", rr,
				"extReceivedRRSN", extReceivedRRSN,
				"packetsInInterval", extReceivedRRSN-s.extLastRRSN,
				"rtpStats", lockedRTPStatsSenderLogEncoder{r},
			)
			continue
		}

		// on every RR, calculate delta since last RR using packet metadata cache
		is := r.getIntervalStats(s.extLastRRSN+1, extReceivedRRSN+1, r.extHighestSN)
		eis := &s.intervalStats
		eis.aggregate(&is)
		if is.packetsNotFound != 0 {
			timeSinceLastRR := time.Since(r.lastRRTime)
			if r.lastRRTime.IsZero() {
				timeSinceLastRR = time.Since(r.startTime)
			}
			r.metadataCacheOverflowCount++
			if (r.metadataCacheOverflowCount-1)%10 == 0 {
				r.logger.Infow(
					"metadata cache overflow",
					"timeSinceLastRR", timeSinceLastRR.String(),
					"receivedRR", rr,
					"extReceivedRRSN", extReceivedRRSN,
					"packetsInInterval", extReceivedRRSN-s.extLastRRSN,
					"intervalStats", is.ToString(),
					"aggregateIntervalStats", eis.ToString(),
					"count", r.metadataCacheOverflowCount,
					"rtpStats", lockedRTPStatsSenderLogEncoder{r},
				)
			}
		}
		s.extLastRRSN = extReceivedRRSN
	}

	r.lastRRTime = time.Now()
	r.lastRR = rr
	return
}

func (r *RTPStatsSender) LastReceiverReportTime() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastRRTime
}

func (r *RTPStatsSender) MaybeAdjustFirstPacketTime(publisherSRData *RTCPSenderReportData, tsOffset uint64) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.initialized || publisherSRData == nil {
		return
	}

	if err, loggingFields := r.maybeAdjustFirstPacketTime(publisherSRData, tsOffset, r.extStartTS); err != nil {
		r.logger.Infow(err.Error(), append(loggingFields, "rtpStats", lockedRTPStatsSenderLogEncoder{r})...)
	}
}

func (r *RTPStatsSender) GetExpectedRTPTimestamp(at time.Time) (expectedTSExt uint64, err error) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if !r.initialized {
		err = errors.New("uninitialized")
		return
	}

	timeDiff := at.Sub(time.Unix(0, r.firstTime))
	expectedRTPDiff := timeDiff.Nanoseconds() * int64(r.params.ClockRate) / 1e9
	expectedTSExt = r.extStartTS + uint64(expectedRTPDiff)
	return
}

func (r *RTPStatsSender) GetRtcpSenderReport(ssrc uint32, publisherSRData *RTCPSenderReportData, tsOffset uint64, passThrough bool) *rtcp.SenderReport {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.initialized || publisherSRData == nil {
		return nil
	}

	timeSincePublisherSRAdjusted := time.Since(publisherSRData.AtAdjusted)
	now := publisherSRData.AtAdjusted.Add(timeSincePublisherSRAdjusted)
	var (
		nowNTP    mediatransportutil.NtpTime
		nowRTPExt uint64
	)
	if passThrough {
		nowNTP = publisherSRData.NTPTimestamp
		nowRTPExt = publisherSRData.RTPTimestampExt - tsOffset
	} else {
		nowNTP = mediatransportutil.ToNtpTime(now)
		nowRTPExt = publisherSRData.RTPTimestampExt - tsOffset + uint64(timeSincePublisherSRAdjusted.Nanoseconds()*int64(r.params.ClockRate)/1e9)
	}

	packetCount := uint32(r.getTotalPacketsPrimary(r.extStartSN, r.extHighestSN) + r.packetsDuplicate + r.packetsPadding)
	octetCount := uint32(r.bytes + r.bytesDuplicate + r.bytesPadding)
	srData := &RTCPSenderReportData{
		NTPTimestamp:    nowNTP,
		RTPTimestamp:    uint32(nowRTPExt),
		RTPTimestampExt: nowRTPExt,
		At:              now,
		AtAdjusted:      now,
		Packets:         packetCount,
		Octets:          octetCount,
	}

	getFields := func() []interface{} {
		return []interface{}{
			"curr", srData,
			"feed", publisherSRData,
			"tsOffset", tsOffset,
			"timeNow", time.Now().String(),
			"now", now.String(),
			"timeSinceHighest", now.Sub(time.Unix(0, r.highestTime)).String(),
			"timeSinceFirst", now.Sub(time.Unix(0, r.firstTime)).String(),
			"timeSincePublisherSRAdjusted", timeSincePublisherSRAdjusted.String(),
			"timeSincePublisherSR", time.Since(publisherSRData.At).String(),
			"nowRTPExt", nowRTPExt,
			"rtpStats", lockedRTPStatsSenderLogEncoder{r},
		}
	}
	if r.srNewest != nil && nowRTPExt >= r.srNewest.RTPTimestampExt {
		timeSinceLastReport := nowNTP.Time().Sub(r.srNewest.NTPTimestamp.Time())
		rtpDiffSinceLastReport := nowRTPExt - r.srNewest.RTPTimestampExt
		windowClockRate := float64(rtpDiffSinceLastReport) / timeSinceLastReport.Seconds()
		if timeSinceLastReport.Seconds() > 0.2 && math.Abs(float64(r.params.ClockRate)-windowClockRate) > 0.2*float64(r.params.ClockRate) {
			r.clockSkewCount++
			if (r.clockSkewCount-1)%100 == 0 {
				fields := append(
					getFields(),
					"timeSinceLastReport", timeSinceLastReport.String(),
					"rtpDiffSinceLastReport", rtpDiffSinceLastReport,
					"windowClockRate", windowClockRate,
					"count", r.clockSkewCount,
				)
				r.logger.Infow("sending sender report, clock skew", fields...)
			}
		}
	}

	if r.srNewest != nil && nowRTPExt < r.srNewest.RTPTimestampExt {
		// If report being generated is behind the last report, skip it.
		// Should not happen.
		r.logger.Infow("sending sender report, out-of-order, skipping", getFields()...)
		return nil
	}

	r.srNewest = srData
	if r.srFirst == nil {
		r.srFirst = r.srNewest
	}

	return &rtcp.SenderReport{
		SSRC:        ssrc,
		NTPTime:     uint64(nowNTP),
		RTPTime:     uint32(nowRTPExt),
		PacketCount: packetCount,
		OctetCount:  octetCount,
	}
}

func (r *RTPStatsSender) DeltaInfo(snapshotID uint32) *RTPDeltaInfo {
	r.lock.Lock()
	defer r.lock.Unlock()

	deltaInfo, err, loggingFields := r.deltaInfo(
		snapshotID,
		r.extStartSN,
		r.extHighestSN,
	)
	if err != nil {
		r.logger.Infow(err.Error(), append(loggingFields, "rtpStats", lockedRTPStatsSenderLogEncoder{r})...)
	}

	return deltaInfo
}

func (r *RTPStatsSender) DeltaInfoSender(senderSnapshotID uint32) *RTPDeltaInfo {
	r.lock.Lock()
	defer r.lock.Unlock()
	if r.lastRRTime.IsZero() {
		return nil
	}

	then, now := r.getAndResetSenderSnapshot(senderSnapshotID)
	if now == nil || then == nil {
		return nil
	}

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := uint32(now.extStartSN - then.extStartSN)
	if packetsExpected > cNumSequenceNumbers {
		r.logger.Warnw(
			"too many packets expected in delta (sender)", nil,
			"startSN", then.extStartSN,
			"endSN", now.extStartSN,
			"packetsExpected", packetsExpected,
			"startTime", startTime.String(),
			"endTime", endTime.String(),
			"duration", endTime.Sub(startTime).String(),
			"rtpStats", lockedRTPStatsSenderLogEncoder{r},
		)
		return nil
	}
	if packetsExpected == 0 {
		// not received RTCP RR (OR) publisher is not producing any data
		return nil
	}

	packetsLost := uint32(now.packetsLost - then.packetsLost)
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}
	packetsLostFeed := uint32(now.packetsLostFeed - then.packetsLostFeed)
	if int32(packetsLostFeed) < 0 {
		packetsLostFeed = 0
	}
	if packetsLost > packetsExpected {
		r.logger.Warnw(
			"unexpected number of packets lost",
			fmt.Errorf(
				"start: %d, end: %d, expected: %d, lost: report: %d, feed: %d",
				then.extStartSN,
				now.extStartSN,
				packetsExpected,
				packetsLost,
				packetsLostFeed,
			),
			"rtpStats", lockedRTPStatsSenderLogEncoder{r},
		)
		packetsLost = packetsExpected
	}

	// discount jitter from publisher side + internal processing
	maxJitter := then.maxJitter - then.maxJitterFeed
	if maxJitter < 0.0 {
		maxJitter = 0.0
	}
	maxJitterTime := maxJitter / float64(r.params.ClockRate) * 1e6

	return &RTPDeltaInfo{
		StartTime:            startTime,
		EndTime:              endTime,
		Packets:              packetsExpected - uint32(now.packetsPadding-then.packetsPadding),
		Bytes:                now.bytes - then.bytes,
		HeaderBytes:          now.headerBytes - then.headerBytes,
		PacketsDuplicate:     uint32(now.packetsDuplicate - then.packetsDuplicate),
		BytesDuplicate:       now.bytesDuplicate - then.bytesDuplicate,
		HeaderBytesDuplicate: now.headerBytesDuplicate - then.headerBytesDuplicate,
		PacketsPadding:       uint32(now.packetsPadding - then.packetsPadding),
		BytesPadding:         now.bytesPadding - then.bytesPadding,
		HeaderBytesPadding:   now.headerBytesPadding - then.headerBytesPadding,
		PacketsLost:          packetsLost,
		PacketsMissing:       packetsLostFeed,
		PacketsOutOfOrder:    uint32(now.packetsOutOfOrder - then.packetsOutOfOrder),
		Frames:               now.frames - then.frames,
		RttMax:               then.maxRtt,
		JitterMax:            maxJitterTime,
		Nacks:                now.nacks - then.nacks,
		Plis:                 now.plis - then.plis,
		Firs:                 now.firs - then.firs,
	}
}

func (r *RTPStatsSender) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	return lockedRTPStatsSenderLogEncoder{r}.MarshalLogObject(e)
}

func (r *RTPStatsSender) String() string {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toString(
		r.extStartSN, r.extHighestSN, r.extStartTS, r.extHighestTS,
		r.packetsLostFromRR,
		r.jitterFromRR, r.maxJitterFromRR,
	)
}

func (r *RTPStatsSender) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.toProto(
		r.extStartSN, r.extHighestSN, r.extStartTS, r.extHighestTS,
		r.packetsLostFromRR,
		r.jitterFromRR, r.maxJitterFromRR,
	)
}

func (r *RTPStatsSender) getAndResetSenderSnapshot(senderSnapshotID uint32) (*senderSnapshot, *senderSnapshot) {
	if !r.initialized || r.lastRRTime.IsZero() {
		return nil, nil
	}

	idx := senderSnapshotID - cFirstSnapshotID
	then := r.senderSnapshots[idx]
	if !then.isValid {
		then = r.initSenderSnapshot(r.startTime, r.extStartSN)
		r.senderSnapshots[idx] = then
	}

	// snapshot now
	now := r.getSenderSnapshot(r.lastRRTime, &then)
	r.senderSnapshots[idx] = now
	return &then, &now
}

func (r *RTPStatsSender) initSenderSnapshot(startTime time.Time, extStartSN uint64) senderSnapshot {
	return senderSnapshot{
		isValid:     true,
		startTime:   startTime,
		extStartSN:  extStartSN,
		extLastRRSN: extStartSN - 1,
	}
}

func (r *RTPStatsSender) getSenderSnapshot(startTime time.Time, s *senderSnapshot) senderSnapshot {
	if s == nil {
		return senderSnapshot{}
	}

	return senderSnapshot{
		isValid:              true,
		startTime:            startTime,
		extStartSN:           s.extLastRRSN + 1,
		bytes:                s.bytes + s.intervalStats.bytes,
		headerBytes:          s.headerBytes + s.intervalStats.headerBytes,
		packetsPadding:       s.packetsPadding + s.intervalStats.packetsPadding,
		bytesPadding:         s.bytesPadding + s.intervalStats.bytesPadding,
		headerBytesPadding:   s.headerBytesPadding + s.intervalStats.headerBytesPadding,
		packetsDuplicate:     r.packetsDuplicate,
		bytesDuplicate:       r.bytesDuplicate,
		headerBytesDuplicate: r.headerBytesDuplicate,
		packetsLostFeed:      r.packetsLost,
		packetsOutOfOrder:    s.packetsOutOfOrder + s.intervalStats.packetsOutOfOrder,
		frames:               s.frames + s.intervalStats.frames,
		nacks:                r.nacks,
		plis:                 r.plis,
		firs:                 r.firs,
		maxRtt:               r.rtt,
		maxJitterFeed:        r.jitter,
		maxJitter:            r.jitterFromRR,
		extLastRRSN:          s.extLastRRSN,
	}
}

func (r *RTPStatsSender) getSnInfoOutOfOrderSlot(esn uint64, ehsn uint64) int {
	offset := int64(ehsn - esn)
	if offset >= cSnInfoSize || offset < 0 {
		// too old OR too new (i. e. ahead of highest)
		return -1
	}

	return int(esn & cSnInfoMask)
}

func (r *RTPStatsSender) setSnInfo(esn uint64, ehsn uint64, pktSize uint16, hdrSize uint8, payloadSize uint16, marker bool, isOutOfOrder bool) {
	var slot int
	if int64(esn-ehsn) < 0 {
		slot = r.getSnInfoOutOfOrderSlot(esn, ehsn)
		if slot < 0 {
			return
		}
	} else {
		slot = int(esn & cSnInfoMask)
	}

	snInfo := &r.snInfos[slot]
	snInfo.pktSize = pktSize
	snInfo.hdrSize = hdrSize
	snInfo.flags = 0
	if marker {
		snInfo.flags |= snInfoFlagMarker
	}
	if payloadSize == 0 {
		snInfo.flags |= snInfoFlagPadding
	}
	if isOutOfOrder {
		snInfo.flags |= snInfoFlagOutOfOrder
	}
}

func (r *RTPStatsSender) clearSnInfos(extStartInclusive uint64, extEndExclusive uint64) {
	if extEndExclusive <= extStartInclusive {
		return
	}

	for esn := extStartInclusive; esn != extEndExclusive; esn++ {
		snInfo := &r.snInfos[esn&cSnInfoMask]
		snInfo.pktSize = 0
		snInfo.hdrSize = 0
		snInfo.flags = 0
	}
}

func (r *RTPStatsSender) isSnInfoLost(esn uint64, ehsn uint64) bool {
	slot := r.getSnInfoOutOfOrderSlot(esn, ehsn)
	if slot < 0 {
		return false
	}

	return r.snInfos[slot].pktSize == 0
}

func (r *RTPStatsSender) getIntervalStats(
	extStartInclusive uint64,
	extEndExclusive uint64,
	ehsn uint64,
) (intervalStats intervalStats) {
	processESN := func(esn uint64, ehsn uint64) {
		slot := r.getSnInfoOutOfOrderSlot(esn, ehsn)
		if slot < 0 {
			intervalStats.packetsNotFound++
			return
		}

		snInfo := &r.snInfos[slot]
		switch {
		case snInfo.pktSize == 0:
			intervalStats.packetsLost++

		case snInfo.flags&snInfoFlagPadding != 0:
			intervalStats.packetsPadding++
			intervalStats.bytesPadding += uint64(snInfo.pktSize)
			intervalStats.headerBytesPadding += uint64(snInfo.hdrSize)

		default:
			intervalStats.packets++
			intervalStats.bytes += uint64(snInfo.pktSize)
			intervalStats.headerBytes += uint64(snInfo.hdrSize)
			if (snInfo.flags & snInfoFlagOutOfOrder) != 0 {
				intervalStats.packetsOutOfOrder++
			}
		}

		if (snInfo.flags & snInfoFlagMarker) != 0 {
			intervalStats.frames++
		}
	}

	for esn := extStartInclusive; esn != extEndExclusive; esn++ {
		processESN(esn, ehsn)
	}
	return
}

func (r *RTPStatsSender) ExtHighestSequenceNumber() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.extHighestSN
}

// -------------------------------------------------------------------

type lockedRTPStatsSenderLogEncoder struct {
	*RTPStatsSender
}

func (r lockedRTPStatsSenderLogEncoder) MarshalLogObject(e zapcore.ObjectEncoder) error {
	if r.RTPStatsSender == nil {
		return nil
	}

	e.AddObject("base", r.rtpStatsBase)
	e.AddUint64("extStartSN", r.extStartSN)
	e.AddUint64("extHighestSN", r.extHighestSN)
	e.AddUint64("extStartTS", r.extStartTS)
	e.AddUint64("extHighestTS", r.extHighestTS)
	e.AddTime("lastRRTime", r.lastRRTime)
	e.AddReflected("lastRR", r.lastRR)
	e.AddUint64("extHighestSNFromRR", r.extHighestSNFromRR)
	e.AddUint64("packetsLostFromRR", r.packetsLostFromRR)
	e.AddFloat64("jitterFromRR", r.jitterFromRR)
	e.AddFloat64("maxJitterFromRR", r.maxJitterFromRR)
	return nil
}
