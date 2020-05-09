package quic

import (
	"sort"
	"time"

	"github.com/lucas-clemente/pstream/ackhandler"
	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/utils"
	"github.com/lucas-clemente/pstream/internal/wire"
)

type scheduler struct {
	pathScheduler func(s *session) (bool, error)
	// XXX Currently round-robin based, inspired from MPTCP scheduler
	//   sent packet count per path
	quotas map[protocol.PathID]uint
	//   stream quota: number of assigned streams per path(except stream 1 and 3)
	numstreams map[protocol.PathID]uint
	//   round robin index for path sending loop
	roundRobinIndexPath uint32
}

type pathOrder struct {
	Key   protocol.PathID
	Value float64
}

func (sch *scheduler) setup(pathScheduler string) {
	sch.quotas = make(map[protocol.PathID]uint)
	sch.numstreams = make(map[protocol.PathID]uint)

	sch.pathScheduler = sch.scheduleToMultiplePaths

}

//   loop to check all retransmit packets for every path(if handshake packet need to be retransmit, return imediately),
//       and put streams into corresponding queue
func (sch *scheduler) getRetransmission(s *session) (hasRetransmission bool, retransmitPacket *ackhandler.Packet, pth *path) {
	// check for retransmissions first
	for {
		// TODO add ability to reinject on another path
		// XXX We need to check on ALL paths if any packet should be first retransmitted
		s.pathsLock.RLock()
	retransmitLoop:
		for _, pthTmp := range s.paths {
			retransmitPacket = pthTmp.sentPacketHandler.DequeuePacketForRetransmission()
			if retransmitPacket != nil {
				pth = pthTmp
				break retransmitLoop
			}
		}
		s.pathsLock.RUnlock()
		if retransmitPacket == nil {
			break
		}
		hasRetransmission = true

		if retransmitPacket.EncryptionLevel != protocol.EncryptionForwardSecure {
			if s.handshakeComplete {
				// Don't retransmit handshake packets when the handshake is complete
				continue
			}
			utils.Debugf("\tDequeueing handshake retransmission for packet 0x%x", retransmitPacket.PacketNumber)
			return
		}
		utils.Debugf("\tDequeueing retransmission of packet 0x%x from path %d", retransmitPacket.PacketNumber, pth.pathID)
		// resend the frames that were in the packet
		for _, frame := range retransmitPacket.GetFramesForRetransmission() {
			switch f := frame.(type) {
			case *wire.StreamFrame:
				s.streamFramer.AddFrameForRetransmission(f)
			case *wire.WindowUpdateFrame:
				// only retransmit WindowUpdates if the stream is not yet closed and the we haven't sent another WindowUpdate with a higher ByteOffset for the stream
				// XXX Should it be adapted to multiple paths?
				currentOffset, err := s.flowControlManager.GetReceiveWindow(f.StreamID)
				if err == nil && f.ByteOffset >= currentOffset {
					s.packer.QueueControlFrame(f, pth)
				}
			case *wire.PathsFrame:
				// Schedule a new PATHS frame to send
				s.schedulePathsFrame()
			default:
				s.packer.QueueControlFrame(frame, pth)
			}
		}
	}
	return
}

//   loop to check all retransmit packets for this path(if handshake packet need to be retransmit, return imediately),
//       and put streams into corresponding queue
func (sch *scheduler) getRetransmissionOfPath(s *session, path *path) (hasRetransmission bool, retransmitPacket *ackhandler.Packet) {
	// check for retransmissions first
	for {
		// TODO add ability to reinject on another path
		// XXX We need to check on ALL paths if any packet should be first retransmitted
		s.pathsLock.RLock()
		retransmitPacket = path.sentPacketHandler.DequeuePacketForRetransmission()
		s.pathsLock.RUnlock()

		if retransmitPacket == nil {
			break
		}
		hasRetransmission = true

		if retransmitPacket.EncryptionLevel != protocol.EncryptionForwardSecure {
			if s.handshakeComplete {
				// Don't retransmit handshake packets when the handshake is complete
				continue
			}
			utils.Debugf("\tDequeueing handshake retransmission for packet 0x%x", retransmitPacket.PacketNumber)
			return
		}
		utils.Debugf("\tDequeueing retransmission of packet 0x%x from path %d", retransmitPacket.PacketNumber, path.pathID)
		// resend the frames that were in the packet, ignore AckFrame and StopWaitingFrame
		for _, frame := range retransmitPacket.GetFramesForRetransmission() {
			switch f := frame.(type) {
			case *wire.StreamFrame:
				s.streamFramer.AddFrameForRetransmission(f)
			case *wire.WindowUpdateFrame:
				// only retransmit WindowUpdates if the stream is not yet closed and the we haven't sent another WindowUpdate with a higher ByteOffset for the stream
				// XXX Should it be adapted to multiple paths?
				currentOffset, err := s.flowControlManager.GetReceiveWindow(f.StreamID)
				if err == nil && f.ByteOffset >= currentOffset {
					s.packer.QueueControlFrame(f, path)
				}
			case *wire.PathsFrame:
				// Schedule a new PATHS frame to send
				s.schedulePathsFrame()
			default:
				s.packer.QueueControlFrame(frame, path)
			}
		}
	}
	return
}
func printStreamInfo(stream *stream) {
	utils.Infof("stream %d: size %d, priority %d\n", stream.streamID, stream.size, stream.priority)
}
func printAllPathsInfo(s *session) {
	for pathID, pth := range s.paths {
		utils.Infof("path %x: bandwidth %d Mbps, rtt %s\n", pathID, float64(pth.bdwStats.GetBandwidth()), pth.rttStats.SmoothedRTT())
	}
}

//assign stream to path
//TODO: if need change schedule results periodically, each time reset the map --stream.pathVolume
func (sch *scheduler) scheduleToMultiplePaths(s *session) (bool, error) {
	assignPath := func(stream *stream) (bool, error) {

		// only assign when the pathID of this stream is not assigned,
		// we assume path won't fail after assignment of a stream
		_, ok := s.streamToPath[stream.streamID]
		if !ok {
			if s.perspective == protocol.PerspectiveClient {
				//client side: assign all streams to lowest RTT path
				pth := sch.findPathLowLatency(s)
				if pth == nil {
					if utils.Debug() {
						utils.Debugf("  fail to assign path to stream %d", stream.streamID)
					}
					windowUpdateFrames := s.getWindowUpdateFrames(false)
					return false, sch.ackRemainingPaths(s, windowUpdateFrames)
				}

				s.streamToPath.Add(stream.streamID, pth.pathID)
				stream.pathVolume[pth.pathID] = 0
				pth.streamIDs = append(pth.streamIDs, stream.streamID)
				if stream.streamID != 1 && stream.streamID != 3 {
					sch.numstreams[pth.pathID]++ //update stream quota
				}
				utils.Infof("ScheduleToMultiplePaths():\n")
				printStreamInfo(stream)
				printAllPathsInfo(s)
				utils.Infof("assigned to path %x\n", pth.pathID)

			} else if s.perspective == protocol.PerspectiveServer {
				//server side
				//1.assign crypto and header stream to lowest RTT path every time
				if stream.streamID == 1 || stream.streamID == 3 {
					pth := sch.findPathLowLatency(s)
					if pth == nil {
						if utils.Debug() {
							utils.Debugf("  fail to assign path to stream %d", stream.streamID)
						}
						windowUpdateFrames := s.getWindowUpdateFrames(false)
						return false, sch.ackRemainingPaths(s, windowUpdateFrames)
					}
					s.streamToPath.Add(stream.streamID, pth.pathID)
					stream.pathVolume[pth.pathID] = 0
					pth.streamIDs = append(pth.streamIDs, stream.streamID)

					utils.Infof("ScheduleToMultiplePaths():\n")
					printStreamInfo(stream)
					printAllPathsInfo(s)
					utils.Infof("assigned to path %x\n", pth.pathID)

				} else {
					//2:  assign other streams according to their priority, path RTT and bandwidth

					//   wait until server created two remote path and all streams come
					if len(s.paths) < 3 {
						return true, nil
					}

					selectedPths := sch.choosePaths(s, stream.streamID, stream.priority.Weight)
					if len(selectedPths) == 0 {
						if utils.Debug() {
							utils.Debugf("  fail to assign path to stream %d", stream.streamID)
						}
						if stream.checksize == true {
							// only assign path when the stream size is known
							// return error under the condition that fail to assign with stream size detected
							windowUpdateFrames := s.getWindowUpdateFrames(false)
							return false, sch.ackRemainingPaths(s, windowUpdateFrames)
						}
						return true, nil

					}
					utils.Infof("ScheduleToMultiplePaths():\n")
					printStreamInfo(stream)
					printAllPathsInfo(s)
					for pth, vol := range selectedPths {
						s.streamToPath.Add(stream.streamID, pth.pathID)
						stream.pathVolume[pth.pathID] = vol
						pth.streamIDs = append(pth.streamIDs, stream.streamID)
						sch.numstreams[pth.pathID]++ //update stream quota
						utils.Infof("assigned to path %x(%s RTT) with volume %f bytes\n", pth.pathID, pth.rttStats.SmoothedRTT(), vol)

					}

				}

			}
		}
		//if this stream is assigned, continue next stream assignment
		return true, nil
	}

	ok := s.streamsMap.sortStreamPriorityOrder()
	if !ok {
		if utils.Debug() {
			utils.Debugf("No new stream to be scheduled\n")
		}
		return true, nil
	}

	//round robin stream for path assginment, prioritize path assignment of stream 1 and 3
	return s.streamsMap.RoundRobinIterateSchedule(assignPath)
}

func (sch *scheduler) iteratePathRoundRobin(s *session) *path {
	if sch.quotas == nil {
		sch.quotas = make(map[protocol.PathID]uint)
	}

	// // XXX Avoid using PathID 0 if there is more than 1 path
	// if len(s.paths) <= 1 {
	// 	if !hasRetransmission && !s.paths[protocol.InitialPathID].SendingAllowed() {
	// 		return nil
	// 	}
	// 	return s.paths[protocol.InitialPathID]
	// }

	// TODO cope with decreasing number of paths (needed?)
	var selectedPath *path
	var lowerQuota, currentQuota uint
	var ok bool

	// Max possible value for lowerQuota at the beginning
	lowerQuota = ^uint(0)

	//pathLoop:
	for pathID, pth := range s.paths {

		// // If this path is potentially failed, do no consider it for sending
		// if pth.potentiallyFailed.Get() {
		// 	continue pathLoop
		// }

		currentQuota, ok = sch.quotas[pathID]
		if !ok {
			sch.quotas[pathID] = 0
			currentQuota = 0
		}

		if currentQuota < lowerQuota {
			selectedPath = pth
			lowerQuota = currentQuota
		}
	}

	return selectedPath

}

func (sch *scheduler) selectPathRoundRobin(s *session, hasRetransmission bool, hasStreamRetransmission bool, fromPth *path) *path {
	if sch.quotas == nil {
		sch.quotas = make(map[protocol.PathID]uint)
	}

	// XXX Avoid using PathID 0 if there is more than 1 path
	if len(s.paths) <= 1 {
		if !hasRetransmission && !s.paths[protocol.InitialPathID].SendingAllowed() {
			return nil
		}
		return s.paths[protocol.InitialPathID]
	}

	// TODO cope with decreasing number of paths (needed?)
	var selectedPath *path
	var lowerQuota, currentQuota uint
	var ok bool

	// Max possible value for lowerQuota at the beginning
	lowerQuota = ^uint(0)

pathLoop:
	for pathID, pth := range s.paths {
		// Don't block path usage if we retransmit, even on another path
		if !hasRetransmission && !pth.SendingAllowed() {
			continue pathLoop
		}

		// If this path is potentially failed, do no consider it for sending
		if pth.potentiallyFailed.Get() {
			continue pathLoop
		}

		// XXX Prevent using initial pathID if multiple paths
		if pathID == protocol.InitialPathID {
			continue pathLoop
		}

		currentQuota, ok = sch.quotas[pathID]
		if !ok {
			sch.quotas[pathID] = 0
			currentQuota = 0
		}

		if currentQuota < lowerQuota {
			selectedPath = pth
			lowerQuota = currentQuota
		}
	}

	return selectedPath

}

func (sch *scheduler) selectPathLowLatency(s *session, hasRetransmission bool, hasStreamRetransmission bool, fromPth *path) *path {
	// XXX Avoid using PathID 0 if there is more than 1 path
	if len(s.paths) <= 1 {
		if !hasRetransmission && !s.paths[protocol.InitialPathID].SendingAllowed() {
			return nil
		}
		return s.paths[protocol.InitialPathID]
	}

	// FIXME Only works at the beginning... Cope with new paths during the connection
	if hasRetransmission && hasStreamRetransmission && fromPth.rttStats.SmoothedRTT() == 0 {
		// Is there any other path with a lower number of packet sent?
		currentQuota := sch.quotas[fromPth.pathID]
		for pathID, pth := range s.paths {
			if pathID == protocol.InitialPathID || pathID == fromPth.pathID {
				continue
			}
			// The congestion window was checked when duplicating the packet
			if sch.quotas[pathID] < currentQuota {
				return pth
			}
		}
	}

	var selectedPath *path
	var lowerRTT time.Duration
	var currentRTT time.Duration
	selectedPathID := protocol.PathID(255)

pathLoop:
	for pathID, pth := range s.paths {
		// Don't block path usage if we retransmit, even on another path
		if !hasRetransmission && !pth.SendingAllowed() {
			continue pathLoop
		}

		// If this path is potentially failed, do not consider it for sending
		if pth.potentiallyFailed.Get() {
			continue pathLoop
		}

		// XXX Prevent using initial pathID if multiple paths
		if pathID == protocol.InitialPathID {
			continue pathLoop
		}

		currentRTT = pth.rttStats.SmoothedRTT()

		// Prefer staying single-path if not blocked by current path
		// Don't consider this sample if the smoothed RTT is 0
		if lowerRTT != 0 && currentRTT == 0 {
			continue pathLoop
		}

		// Case if we have multiple paths unprobed, prefer path with smaller quota(packet sent per path)
		if currentRTT == 0 {
			currentQuota, ok := sch.quotas[pathID]
			if !ok {
				sch.quotas[pathID] = 0
				currentQuota = 0
			}
			lowerQuota, _ := sch.quotas[selectedPathID]
			if selectedPath != nil && currentQuota > lowerQuota {
				continue pathLoop
			}
		}

		if currentRTT != 0 && lowerRTT != 0 && selectedPath != nil && currentRTT >= lowerRTT {
			continue pathLoop
		}

		// Update
		lowerRTT = currentRTT
		selectedPath = pth
		selectedPathID = pathID
	}

	return selectedPath
}

//   find the path with lowest latency ; if multiple path unprobed, find path with lowest quota
func (sch *scheduler) findPathLowLatency(s *session) *path {
	// XXX Avoid using PathID 0 if there is more than 1 path
	if len(s.paths) <= 1 {
		if !s.paths[protocol.InitialPathID].SendingAllowed() {
			return nil
		}
		return s.paths[protocol.InitialPathID]
	}

	var selectedPath *path
	var lowerRTT time.Duration
	var currentRTT time.Duration
	selectedPathID := protocol.PathID(255)

pathLoop:
	for pathID, pth := range s.paths {

		if !pth.SendingAllowed() {
			continue pathLoop
		}

		// If this path is potentially failed, do not consider it for sending
		if pth.potentiallyFailed.Get() {
			continue pathLoop
		}

		// XXX Prevent using initial pathID if multiple paths
		if pathID == protocol.InitialPathID {
			continue pathLoop
		}

		currentRTT = pth.rttStats.SmoothedRTT()

		// Prefer staying single-path if not blocked by current path
		// Don't consider this sample if the smoothed RTT is 0
		if lowerRTT != 0 && currentRTT == 0 {
			continue pathLoop
		}

		// Case if we have multiple paths unprobed, chose one path with lowest number of packet sent
		if currentRTT == 0 {
			currentQuota, ok := sch.quotas[pathID]
			if !ok {
				sch.quotas[pathID] = 0
				currentQuota = 0
			}
			lowerQuota, _ := sch.quotas[selectedPathID]
			if selectedPath != nil && currentQuota > lowerQuota {
				continue pathLoop
			}
		}

		if currentRTT != 0 && lowerRTT != 0 && selectedPath != nil && currentRTT >= lowerRTT {
			continue pathLoop
		}

		// Update
		lowerRTT = currentRTT
		selectedPath = pth
		selectedPathID = pathID
	}

	return selectedPath
}

//   return available path set
func (sch *scheduler) checkPathQuota(s *session) map[protocol.PathID]*path {
	if sch.numstreams == nil {
		sch.numstreams = make(map[protocol.PathID]uint)
	}

	avalPath := make(map[protocol.PathID]*path)
	var pathID protocol.PathID

	// Max possible value for lowerQuota at the beginning
	lowerQuota := ^uint(0)

	for pthID := range s.paths {
		_, ok := sch.numstreams[pthID]
		if !ok {
			sch.numstreams[pthID] = 0
		}
	}

	for pthID, quota := range sch.numstreams {
		if pthID == protocol.InitialPathID {
			continue
		}
		if quota < lowerQuota {
			lowerQuota = quota
			pathID = pthID
		}
	}

	avalPath[pathID] = s.paths[pathID]

	for pthID, quota := range sch.numstreams {
		if pthID == protocol.InitialPathID {
			continue
		}
		if quota == lowerQuota {
			avalPath[pthID] = s.paths[pthID]
		}
	}
	avalPath[protocol.InitialPathID] = s.paths[protocol.InitialPathID]

	return avalPath
}

func (sch *scheduler) choosePath(s *session, strID protocol.StreamID, priority uint8) *path {
	// XXX Avoid using PathID 0 if there is more than 1 path
	if len(s.paths) <= 1 {
		if !s.paths[protocol.InitialPathID].SendingAllowed() {
			return nil
		}
		return s.paths[protocol.InitialPathID]
	}
	stream := s.streamsMap.streams[strID]

	//  assign path only if the size of a flow is detected
	if stream.checksize == false {
		stream.size = stream.lenOfDataForWriting() //return Byte
		if stream.size != 0 {
			stream.checksize = true
			if utils.Debug() {
				//TODO: Stream size limited with 32768 bytes
				utils.Debugf("Detected: Stream %d with file size %d bytes\n", strID, stream.size)
			}

		} else {
			if utils.Debug() {
				utils.Debugf("Not Detected: Stream %d not detected file size \n", strID)
			}
			return nil //size value undetected, do not assign path

		}
	}

	var selectedPath *path
	var lowerTime float64
	var currentTime float64 // second

pathLoop:
	for pathID, pth := range s.paths {

		if !pth.SendingAllowed() {
			continue pathLoop
		}

		// If this path is potentially failed, do not consider it for sending
		if pth.potentiallyFailed.Get() {
			continue pathLoop
		}

		// XXX Prevent using initial pathID if multiple paths
		if pathID == protocol.InitialPathID {
			continue pathLoop
		}

		//  calculate estimated transmission time of this stream on this path

		prioritySum := float32(0)
		for _, sid := range pth.streamIDs {
			//    we ignore stream 1 and 3 as they are treated with absolute priority
			if sid == 1 || sid == 3 {
				continue
			}
			str := s.streamsMap.streams[sid]
			prioritySum += float32(str.priority.Weight)

		}

		bandwidthShare := (float64(priority) / (float64(priority) + float64(prioritySum))) * float64(pth.bdwStats.GetBandwidth())
		//size: Byte
		currentTime = (float64(stream.size)*8)/(bandwidthShare*1048576) + (pth.rttStats.SmoothedRTT().Seconds() / 2)
		//bandwidthShare: Mbps, rtt: ms

		utils.Infof("path %d, rtt %s ms,fullbandwidth %d Mbps, prioritySum %f", pth.pathID, pth.rttStats.SmoothedRTT().String(), pth.bdwStats.GetBandwidth(), prioritySum)
		utils.Infof("stream %d, priority %d, size %d Byte, bandwidthshare %f Mbps, estimated time %f ", strID, priority, stream.size, bandwidthShare, currentTime)

		if currentTime != 0 && lowerTime != 0 && selectedPath != nil && currentTime >= lowerTime {
			continue pathLoop
		}

		// Update
		lowerTime = currentTime
		selectedPath = pth
	}

	return selectedPath
}

//choosePaths chooses paths for normal streams, and assign certain amount of data (/byte) to be transmitted on each path
func (sch *scheduler) choosePaths(s *session, strID protocol.StreamID, priority uint8) (selectedPaths map[*path]float64) {

	stream := s.streamsMap.streams[strID]

	//  assign path only if the size of a flow is detected
	if stream.checksize == false {
		stream.size = stream.lenOfDataForWriting() //return Byte
		if stream.size != 0 {
			stream.checksize = true

			//TODO: Stream size limited with 32768 bytes
			utils.Infof("Detected: Stream %d with file size %d bytes\n", strID, stream.size)

		} else {
			utils.Infof("Not Detected: Stream %d not detected file size \n", strID)

			return nil //size value undetected, do not assign path

		}
	}
	// var lowerTime float64
	// var currentTime float64 // second
	var avalPaths []*path
	var sortedPathsBdw []protocol.PathID // maps are unordered, thus use array
	selectedPaths = make(map[*path]float64)
	pathsOwd := make(map[protocol.PathID]float64)
	pathsBdw := make(map[protocol.PathID]float64)
	pathsVolume := make(map[protocol.PathID]float64)
	volume := float64(stream.size) * 8 //bit
	var proportionStep float64

	// XXX Avoid using PathID 0 if there is more than 1 path
	if len(s.paths) <= 1 {
		if !s.paths[protocol.InitialPathID].SendingAllowed() {
			return nil
		}
		selectedPaths[s.paths[protocol.InitialPathID]] = float64(stream.size) // assign all data of the stream onto the only path
		return selectedPaths
	}

	//filter unavailable paths
pathLoop:
	for pathID, pth := range s.paths {

		if !pth.SendingAllowed() {
			continue pathLoop
		}

		// If this path is potentially failed, do not consider it for sending
		if pth.potentiallyFailed.Get() {
			continue pathLoop
		}

		// XXX Prevent using initial pathID if multiple paths
		if pathID == protocol.InitialPathID {
			continue pathLoop
		}
		avalPaths = append(avalPaths, pth)
	}

	for _, pth := range avalPaths {

		//----------- priority sum of already scheduled stream on this path ------
		prioritySum := float32(0)
		for _, sid := range pth.streamIDs {
			//    we ignore stream 1 and 3 as they are treated with absolute priority
			if sid == 1 || sid == 3 {
				continue
			}

			// prioritySum += float32(stream.priority.Weight)

			str := s.streamsMap.streams[sid]
			prioritySum += float32(str.priority.Weight)

		}

		pathsBdw[pth.pathID] = (float64(priority) / (float64(priority) + float64(prioritySum))) * float64(pth.bdwStats.GetBandwidth()) * 1048576 //bit
		//------------------
		//pathsBdw[pth.pathID] =  float64(pth.bdwStats.GetBandwidth() * 1048576) //bit

		pathsOwd[pth.pathID] = float64(pth.rttStats.SmoothedRTT().Seconds() / 2) //second
		pathsVolume[pth.pathID] = 0

		utils.Infof("path %d, shared bandwidth %f Mbps of stream %d, owd %f s\n", pth.pathID, pathsBdw[pth.pathID]/1048576, strID, pathsOwd[pth.pathID])

	}

	var orders []pathOrder
	for pid, owd := range pathsOwd {
		orders = append(orders, pathOrder{pid, owd})
	}

	sort.Slice(orders, func(i, j int) bool {
		return orders[i].Value < orders[j].Value
	})
	if utils.Debug() {
		utils.Debugf("----- Step 1: ----- ")
		utils.Debugf("sort paths by ascending order of one-way delay\n")
	}
	for _, order := range orders {
		sortedPathsBdw = append(sortedPathsBdw, order.Key)
		if utils.Debug() {
			utils.Debugf("order.Key: %d, order.Value: %f\n", order.Key, order.Value)
		}
	}

	if utils.Debug() {
		utils.Debugf("----- Step 2: ----- ")
		utils.Debugf("close the gap between paths\n")
	}
	length := len(avalPaths)
	for i := 0; i < length-1; i++ {
		pathA := sortedPathsBdw[i]
		pathB := sortedPathsBdw[i+1]

		k := i
		bdwSum := float64(0)

		for k >= 0 {
			bdwSum += pathsBdw[sortedPathsBdw[k]]
			k--
		}

		owdGap := pathsOwd[pathB] - pathsOwd[pathA]
		if owdGap != 0 {
			if utils.Debug() {
				utils.Debugf("----- Step 2: ----- ")
				utils.Debugf("Close the gap between Path %d and Path %d\n", pathA, pathB)
			}
			gap := float64(owdGap * bdwSum)
			k = i
			if volume > gap {
				for k >= 0 {
					proportionStep = float64(owdGap * pathsBdw[sortedPathsBdw[k]])
					pathsVolume[sortedPathsBdw[k]] += proportionStep
					volume -= proportionStep
					if volume <= 0 {
						for k, v := range pathsVolume {
							time := v/float64(pathsBdw[k]) + float64(pathsOwd[k])
							if utils.Debug() {
								utils.Debugf("----- Step 2: ----- ")
								utils.Debugf("Path: %d, bandwidth %f bps, volume %f bits, time %f s\n", k, pathsBdw[k], v, time)
							}
						}
						if utils.Debug() {
							utils.Debugf("----- Step 2: ----- ")
							utils.Debugf("no volume left\n")
						}
						break
					}
					k--
				}
			} else {
				cutted := float64(0)
				for k >= 0 {
					proportionStep = volume * float64(pathsBdw[sortedPathsBdw[k]]) / float64(bdwSum)
					pathsVolume[sortedPathsBdw[k]] += proportionStep
					cutted += proportionStep
					k--
				}
				volume -= cutted
				if volume <= 0 {
					for k, v := range pathsVolume {
						time := v/float64(pathsBdw[k]) + float64(pathsOwd[k])
						if utils.Debug() {
							utils.Debugf("----- Step 2: ----- ")
							utils.Debugf("Path: %d, bandwidth %f bps, volume %f bits, time %f s\n", k, pathsBdw[k], v, time)
						}
					}
					if utils.Debug() {
						utils.Debugf("----- Step 2: ----- ")
						utils.Debugf("no volume left\n")
					}
					break
				}

			}

			for k, v := range pathsVolume {
				time := v/float64(pathsBdw[k]) + float64(pathsOwd[k])
				if utils.Debug() {
					utils.Debugf("----- Step 2: ----- ")
					utils.Debugf("Path: %d, bandwidth %f bps, volume %f bits, time %f s\n", k, pathsBdw[k], v, time)
				}
			}
		} else {
			break
		}
	}

	//Step 3: distribute proportionally according to bandwidth
	if volume > 0 {
		if utils.Debug() {
			utils.Debugf("----- Step 3: ----- ")
			utils.Debugf("The rest volume %f bits\n", volume)
			utils.Debugf("----- Step 3: ----- ")

			utils.Debugf("distribute proportionally according to bandwidth\n\n")
		}
		all := float64(0)
		for _, v := range pathsBdw {
			all += v
		}

		for _, pth := range avalPaths {
			restShare := volume * float64(pathsBdw[pth.pathID]) / float64(all)
			pathsVolume[pth.pathID] += restShare
		}

	}
	if utils.Debug() {
		utils.Debugf("----- Step 3: ----- ")
		utils.Debugf("Final assignment result:\n")
	}
	for k, v := range pathsVolume {
		time := v/float64(pathsBdw[k]) + float64(pathsOwd[k])
		if utils.Debug() {
			utils.Debugf("Path: %d, volume %f bits, time %f s\n", k, v, time)
		}
		if v > 0 {
			selectedPaths[s.paths[k]] = v / 8
		}

	}

	return selectedPaths
}

//   find path for stream according to priority : highest priority to smallest rtt path, second high priority to second small rtt path(controlled by numstreams per path)
//      numstream per path round robin > path rtt > numpacket per path round robin
func (sch *scheduler) findPath(s *session, strID protocol.StreamID, priority uint8) *path {
	// XXX Avoid using PathID 0 if there is more than 1 path
	if len(s.paths) <= 1 {
		if !s.paths[protocol.InitialPathID].SendingAllowed() {
			return nil
		}
		return s.paths[protocol.InitialPathID]
	}

	var selectedPath *path
	var lowerRTT time.Duration
	var currentRTT time.Duration
	selectedPathID := protocol.PathID(255)

	//  more than 1 pth, narrow down path set
	avalPath := sch.checkPathQuota(s)

pathLoop:
	for pathID, pth := range avalPath {

		if !pth.SendingAllowed() {
			continue pathLoop
		}

		// If this path is potentially failed, do not consider it for sending
		if pth.potentiallyFailed.Get() {
			continue pathLoop
		}

		// XXX Prevent using initial pathID if multiple paths
		if pathID == protocol.InitialPathID {
			continue pathLoop
		}

		currentRTT = pth.rttStats.SmoothedRTT()

		// Prefer staying single-path if not blocked by current path
		// Don't consider this sample if the smoothed RTT is 0
		if lowerRTT != 0 && currentRTT == 0 {
			continue pathLoop
		}

		// Case if we have multiple paths unprobed, chose one path with lowest number of packet sent
		if currentRTT == 0 {
			currentQuota, ok := sch.quotas[pathID]
			if !ok {
				sch.quotas[pathID] = 0
				currentQuota = 0
			}
			lowerQuota, _ := sch.quotas[selectedPathID]
			if selectedPath != nil && currentQuota > lowerQuota {
				continue pathLoop
			}
		}

		if currentRTT != 0 && lowerRTT != 0 && selectedPath != nil && currentRTT >= lowerRTT {
			continue pathLoop
		}

		// Update
		lowerRTT = currentRTT
		selectedPath = pth
		selectedPathID = pathID
	}

	return selectedPath
}

// Lock of s.paths must be held
func (sch *scheduler) selectPath(s *session, hasRetransmission bool, hasStreamRetransmission bool, fromPth *path) *path {
	// XXX Currently round-robin
	// TODO select the right scheduler dynamically
	return sch.selectPathLowLatency(s, hasRetransmission, hasStreamRetransmission, fromPth)
	// return sch.selectPathRoundRobin(s, hasRetransmission, hasStreamRetransmission, fromPth)
}

// Lock of s.paths must be free (in case of log print)
func (sch *scheduler) performPacketSending(s *session, windowUpdateFrames []*wire.WindowUpdateFrame, pth *path) (*ackhandler.Packet, bool, error) {
	// add a retransmittable frame
	if pth.sentPacketHandler.ShouldSendRetransmittablePacket() {
		s.packer.QueueControlFrame(&wire.PingFrame{}, pth)
	}
	packet, err := s.packer.PackPacketOfPath(pth)
	if err != nil || packet == nil {

		return nil, false, err
	}
	if err = s.sendPackedPacket(packet, pth); err != nil {
		return nil, false, err
	}

	// send every window update twice
	for _, f := range windowUpdateFrames {
		s.packer.QueueControlFrame(f, pth)
	}

	// Packet sent, so update its quota
	sch.quotas[pth.pathID]++

	// Provide some logging if it is the last packet
	for _, frame := range packet.frames {
		switch frame := frame.(type) {
		case *wire.StreamFrame:
			if frame.FinBit {
				// Last packet to send on the stream, print stats
				s.pathsLock.RLock()
				utils.Infof("Info for stream %d of %x", frame.StreamID, s.connectionID)
				for pathID, pth := range s.paths {
					sntPkts, sntRetrans, sntLost := pth.sentPacketHandler.GetStatistics()
					rcvPkts := pth.receivedPacketHandler.GetStatistics()
					utils.Infof("Path %x: sent %d retrans %d lost %d; rcv %d rtt %v", pathID, sntPkts, sntRetrans, sntLost, rcvPkts, pth.rttStats.SmoothedRTT())
				}
				s.pathsLock.RUnlock()
			}
		default:
		}
	}

	pkt := &ackhandler.Packet{
		PacketNumber:    packet.number,
		Frames:          packet.frames,
		Length:          protocol.ByteCount(len(packet.raw)),
		EncryptionLevel: packet.encryptionLevel,
	}

	return pkt, true, nil
}
func (sch *scheduler) performPacketSendingStream(s *session, windowUpdateFrames []*wire.WindowUpdateFrame, pth *path, sid protocol.StreamID) (*ackhandler.Packet, bool, error) {
	// add a retransmittable frame
	if pth.sentPacketHandler.ShouldSendRetransmittablePacket() {
		s.packer.QueueControlFrame(&wire.PingFrame{}, pth)
	}
	packet, err := s.packer.PackPacketOfStream(pth, sid)
	if err != nil || packet == nil {
		return nil, false, err
	}
	if err = s.sendPackedPacketOfStream(packet, pth, sid); err != nil {
		return nil, false, err
	}

	// send every window update twice
	for _, f := range windowUpdateFrames {
		s.packer.QueueControlFrame(f, pth)
	}

	// Packet sent, so update its quota
	sch.quotas[pth.pathID]++

	// Provide some logging if it is the last packet
	for _, frame := range packet.frames {
		switch frame := frame.(type) {
		case *wire.StreamFrame:
			if frame.FinBit {
				// Last packet to send on the stream, print stats
				s.pathsLock.RLock()
				utils.Infof("Info for stream %d of %x", frame.StreamID, s.connectionID)
				for pathID, pth := range s.paths {
					sntPkts, sntRetrans, sntLost := pth.sentPacketHandler.GetStatistics()
					rcvPkts := pth.receivedPacketHandler.GetStatistics()
					utils.Infof("Path %x: sent %d retrans %d lost %d; rcv %d rtt %v", pathID, sntPkts, sntRetrans, sntLost, rcvPkts, pth.rttStats.SmoothedRTT())
				}
				s.pathsLock.RUnlock()
			}
		default:
		}
	}

	pkt := &ackhandler.Packet{
		PacketNumber:    packet.number,
		Frames:          packet.frames,
		Length:          protocol.ByteCount(len(packet.raw)),
		EncryptionLevel: packet.encryptionLevel,
	}

	return pkt, true, nil
}

/*
func (sch *scheduler) performACKPacketSending(s *session, pth *path) (*ackhandler.Packet, bool, error) {

	packet, err := s.packer.PackACKPacketOfPath(pth)
	if err != nil || packet == nil {
		return nil, false, err
	}
	if err = s.sendPackedPacket(packet, pth); err != nil {
		return nil, false, err
	}

	// Packet sent, so update its quota
	sch.quotas[pth.pathID]++

	// Provide some logging if it is the last packet
	for _, frame := range packet.frames {
		switch frame := frame.(type) {
		case *wire.StreamFrame:
			if frame.FinBit {
				// Last packet to send on the stream, print stats
				s.pathsLock.RLock()
				utils.Infof("Info for stream %d of %x", frame.StreamID, s.connectionID)
				for pathID, pth := range s.paths {
					sntPkts, sntRetrans, sntLost := pth.sentPacketHandler.GetStatistics()
					rcvPkts := pth.receivedPacketHandler.GetStatistics()
					utils.Infof("Path %x: sent %d retrans %d lost %d; rcv %d rtt %v", pathID, sntPkts, sntRetrans, sntLost, rcvPkts, pth.rttStats.SmoothedRTT())
				}
				s.pathsLock.RUnlock()
			}
		default:
		}
	}

	pkt := &ackhandler.Packet{
		PacketNumber:    packet.number,
		Frames:          packet.frames,
		Length:          protocol.ByteCount(len(packet.raw)),
		EncryptionLevel: packet.encryptionLevel,
	}

	return pkt, true, nil
}
*/
// Lock of s.paths must be free
func (sch *scheduler) ackRemainingPaths(s *session, totalWindowUpdateFrames []*wire.WindowUpdateFrame) error {
	// Either we run out of data, or CWIN of usable paths are full
	// Send ACKs on paths not yet used, if needed. Either we have no data to send and
	// it will be a pure ACK, or we will have data in it, but the CWIN should then
	// not be an issue.
	s.pathsLock.RLock()
	defer s.pathsLock.RUnlock()
	// get WindowUpdate frames
	// this call triggers the flow controller to increase the flow control windows, if necessary
	windowUpdateFrames := totalWindowUpdateFrames
	if len(windowUpdateFrames) == 0 {
		windowUpdateFrames = s.getWindowUpdateFrames(s.peerBlocked)
	}
	for _, pthTmp := range s.paths {
		ackTmp := pthTmp.GetAckFrame()
		for _, wuf := range windowUpdateFrames {
			s.packer.QueueControlFrame(wuf, pthTmp)
		}
		if ackTmp != nil || len(windowUpdateFrames) > 0 {
			if pthTmp.pathID == protocol.InitialPathID && ackTmp == nil {
				continue
			}
			swf := pthTmp.GetStopWaitingFrame(false)
			if swf != nil {
				s.packer.QueueControlFrame(swf, pthTmp)
			}
			s.packer.QueueControlFrame(ackTmp, pthTmp)
			// XXX (QDC) should we instead call PackPacket to provides WUFs?
			var packet *packedPacket
			var err error
			if ackTmp != nil {
				// Avoid internal error bug
				packet, err = s.packer.PackAckPacket(pthTmp)
			} else {
				//   change this also into only pack path related packet
				packet, err = s.packer.PackPacketOfPath(pthTmp)
			}
			if err != nil {
				return err
			}
			err = s.sendPackedPacket(packet, pthTmp)
			if err != nil {
				return err
			}
		}
	}
	s.peerBlocked = false
	return nil
}

func (sch *scheduler) ackRemainingOnePath(pthTmp *path, s *session, totalWindowUpdateFrames []*wire.WindowUpdateFrame) error {
	// Either we run out of data, or CWIN of usable paths are full
	// Send ACKs on paths not yet used, if needed. Either we have no data to send and
	// it will be a pure ACK, or we will have data in it, but the CWIN should then
	// not be an issue.
	//s.pathsLock.RLock()
	//defer s.pathsLock.RUnlock()
	// get WindowUpdate frames
	// this call triggers the flow controller to increase the flow control windows for streams and connection, if necessary
	// if utils.Debug() {
	// 	utils.Debugf(" ackRemainingOnePath: before s.getWindowUpdateFrames() ")
	// }
	windowUpdateFrames := totalWindowUpdateFrames
	if len(windowUpdateFrames) == 0 {
		windowUpdateFrames = s.getWindowUpdateFrames(s.peerBlocked)
	}

	// if utils.Debug() {
	// 	utils.Debugf(" ackRemainingOnePath: before pthTmp.GetAckFrame() ")
	// }
	ackTmp := pthTmp.GetAckFrame()
	for _, wuf := range windowUpdateFrames {
		s.packer.QueueControlFrame(wuf, pthTmp)
	}
	if ackTmp != nil || len(windowUpdateFrames) > 0 {
		if pthTmp.pathID == protocol.InitialPathID && ackTmp == nil {
			return nil
		}
		swf := pthTmp.GetStopWaitingFrame(false)
		if swf != nil {
			s.packer.QueueControlFrame(swf, pthTmp)
		}
		s.packer.QueueControlFrame(ackTmp, pthTmp)
		// XXX (QDC) should we instead call PackPacket to provides WUFs?
		var packet *packedPacket
		var err error
		if ackTmp != nil {
			// Avoid internal error bug

			if utils.Debug() {
				utils.Debugf(" ackRemainingOnePath: before s.packer.PackAckPacket(pthTmp) ")
			}
			packet, err = s.packer.PackAckPacket(pthTmp)
		} else {
			//   TODO:  change this also into only pack path related packet
			if utils.Debug() {
				utils.Debugf(" ackRemainingOnePath: before s.packer.PackPacketOfPath(pthTmp)")
			}
			packet, err = s.packer.PackPacketOfPath(pthTmp)
		}
		if err != nil {
			return err
		}

		// if utils.Debug() {
		// 	utils.Debugf(" ackRemainingOnePath: before s.sendPackedPacket(packet, pthTmp)")
		// }
		err = s.sendPackedPacket(packet, pthTmp)
		if err != nil {
			return err
		}

		// if utils.Debug() {
		// 	utils.Debugf(" ackRemainingOnePath: after! s.sendPackedPacket(packet, pthTmp)")
		// }
	}

	s.peerBlocked = false
	return nil
}

func (sch *scheduler) sendPacket(s *session) error {

	//   assign stream to path.
	// path might not be assigned due to initial path congestion limited and we need to send ACK frames when congestion limited
	_, err := sch.pathScheduler(s)

	if err != nil {
		return err
	}

	var path *path

	// TODO: separate windowUpdateFrames for different path
	// get WindowUpdate frames
	// this call triggers the flow controller to increase the flow control windows, if necessary
	windowUpdateFrames := s.getWindowUpdateFrames(false)
	for _, wuf := range windowUpdateFrames {
		s.packer.QueueControlFrame(wuf, path)
	}

	//  assgin path id
	numOfPath := uint32(len(s.paths))

	startIndex := sch.roundRobinIndexPath

	// Repeatedly try sending until all path don't have any more data, or run out of the congestion window
	for {
		hasWindows := false
		pathsent := false

	PATHLOOP:
		for i := uint32(0); i < numOfPath; i++ {
			pid := s.openPaths[(i+startIndex)%numOfPath]

			path = s.paths[pid]

			// Update leastUnacked value of current path
			path.SetLeastUnacked(path.sentPacketHandler.GetLeastUnacked())

			streamNum := len(path.streamIDs)

			//test begin
			if utils.Debug() {
				utils.Debugf("In test: path %d, rtt %s ms,fullbandwidth %d Mbps", path.pathID, path.rttStats.SmoothedRTT().String(), path.bdwStats.GetBandwidth())
			}
			//test end

			//path with stream, send data
			if streamNum > 0 {

				for streamNum > 0 { //   to provide fairness concern between paths
					if utils.Debug() {
						utils.Debugf("Path %d, sending the %d round", path.pathID, streamNum)
					}
					hasWindows = hasWindows || path.SendingAllowed()

					// the path runs out of window, continue to next path
					if !path.SendingAllowed() {
						if utils.Debug() {
							utils.Debugf("  sending not allowed on path %d", path.pathID)
						}

						sch.roundRobinIndexPath = (sch.roundRobinIndexPath + 1) % numOfPath

						continue PATHLOOP
					}

					//   We first check for retransmissions of this path in path.sentPacketHandler and put retransmit frames into streamframer
					hasRetransmission, retransmitHandshakePacket := sch.getRetransmissionOfPath(s, path)
					// XXX There might still be some stream frames to be retransmitted
					hasStreamRetransmission := s.streamFramer.HasFramesForRetransmission()

					// If we have an handshake packet retransmission, do it directly and continue to send data of this path
					if hasRetransmission && retransmitHandshakePacket != nil {
						s.packer.QueueControlFrame(path.sentPacketHandler.GetStopWaitingFrame(true), path)
						packet, err := s.packer.PackHandshakeRetransmission(retransmitHandshakePacket, path)
						if err != nil {
							return err
						}
						if err = s.sendPackedPacket(packet, path); err != nil {
							return err
						}
					}

					// XXX Some automatic ACK generation should be done someway
					var ack *wire.AckFrame

					ack = path.GetAckFrame()
					if ack != nil {
						s.packer.QueueControlFrame(ack, path)
					}
					if ack != nil || hasStreamRetransmission {
						swf := path.sentPacketHandler.GetStopWaitingFrame(hasStreamRetransmission)
						if swf != nil {
							s.packer.QueueControlFrame(swf, path)
						}
					}

					// Also add CLOSE_PATH frames, if any
					for cpf := s.streamFramer.PopClosePathFrame(); cpf != nil; cpf = s.streamFramer.PopClosePathFrame() {
						s.packer.QueueControlFrame(cpf, path)
					}

					// Also add ADD ADDRESS frames, if any
					for aaf := s.streamFramer.PopAddAddressFrame(); aaf != nil; aaf = s.streamFramer.PopAddAddressFrame() {
						s.packer.QueueControlFrame(aaf, path)
					}

					// Also add PATHS frames, if any
					for pf := s.streamFramer.PopPathsFrame(); pf != nil; pf = s.streamFramer.PopPathsFrame() {
						s.packer.QueueControlFrame(pf, path)
					}

					_, sent, err := sch.performPacketSending(s, windowUpdateFrames, path)
					if err != nil {
						return err
					}
					windowUpdateFrames = nil
					pathsent = pathsent || sent

					if !sent {
						// this stream sending empty packets, continue to next path
						if utils.Debug() {
							utils.Debugf("  sending empty packets on path %d", path.pathID)
						}
						sch.roundRobinIndexPath = (sch.roundRobinIndexPath + 1) % numOfPath

						continue PATHLOOP
					}

					//  disable duplicate sending on other path
					streamNum--
				}
			} else { // path without stream, ack path
				if utils.Debug() {
					utils.Debugf("  path %d without stream ", path.pathID)
				}
				sch.roundRobinIndexPath = (sch.roundRobinIndexPath + 1) % numOfPath

				continue PATHLOOP

			}

			sch.roundRobinIndexPath = (sch.roundRobinIndexPath + 1) % numOfPath
		}

		//all path (with stream) sending emptypackets or all path (with stream) run out of window
		if !pathsent || !hasWindows {

			return sch.ackRemainingPaths(s, windowUpdateFrames)

		}
	}
}
