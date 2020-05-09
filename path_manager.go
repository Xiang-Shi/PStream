package quic

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/lucas-clemente/pstream/congestion"
	"github.com/lucas-clemente/pstream/internal/protocol"
	"github.com/lucas-clemente/pstream/internal/utils"
	"github.com/lucas-clemente/pstream/internal/wire"
)

type pathManager struct {
	pconnMgr  *pconnManager
	sess      *session
	nxtPathID protocol.PathID
	// Number of paths, excluding the initial one
	nbPaths uint8

	remoteAddrs4 []net.UDPAddr
	remoteAddrs6 []net.UDPAddr

	advertisedLocAddrs map[string]bool

	// TODO (QDC): find a cleaner way
	oliaSenders map[protocol.PathID]*congestion.OliaSender

	handshakeCompleted chan struct{}
	runClosed          chan struct{}
	timer              *time.Timer
}

func (pm *pathManager) setup(conn connection) {
	// Initial PathID is 0
	// PathIDs of client-initiated paths are even
	// those of server-initiated paths odd
	if pm.sess.perspective == protocol.PerspectiveClient {
		pm.nxtPathID = 1
	} else {
		pm.nxtPathID = 2
	}

	pm.remoteAddrs4 = make([]net.UDPAddr, 0)
	pm.remoteAddrs6 = make([]net.UDPAddr, 0)
	pm.advertisedLocAddrs = make(map[string]bool)
	pm.handshakeCompleted = make(chan struct{}, 1)
	pm.runClosed = make(chan struct{}, 1)
	pm.timer = time.NewTimer(0)
	pm.nbPaths = 0

	pm.oliaSenders = make(map[protocol.PathID]*congestion.OliaSender)

	// Setup the first path of the connection
	pm.sess.paths[protocol.InitialPathID] = &path{
		pathID: protocol.InitialPathID,
		sess:   pm.sess,
		conn:   conn,
	}

	// Setup this first path
	pm.sess.paths[protocol.InitialPathID].setup(pm.oliaSenders)
	pm.sess.openPaths = append(pm.sess.openPaths, protocol.InitialPathID)

	// With the initial path, get the remoteAddr to create paths accordingly
	if conn.RemoteAddr() != nil {
		remAddr, err := net.ResolveUDPAddr("udp", conn.RemoteAddr().String())
		if err != nil {
			utils.Errorf("path manager: encountered error while parsing remote addr: %v", remAddr)
		}

		if remAddr.IP.To4() != nil {
			pm.remoteAddrs4 = append(pm.remoteAddrs4, *remAddr)
		} else {
			pm.remoteAddrs6 = append(pm.remoteAddrs6, *remAddr)
		}
	}

	// Launch the path manager
	go pm.run()
}

func (pm *pathManager) run() {
	// Close immediately if requested
	select {
	case <-pm.runClosed:
		return
	case <-pm.handshakeCompleted:
		if pm.sess.createPaths {
			err := pm.createPaths()
			if err != nil {
				pm.closePaths()
				return
			}
		}
	}

runLoop:
	for {
		select {
		case <-pm.runClosed:
			break runLoop
		case <-pm.pconnMgr.changePaths:
			if pm.sess.createPaths {
				pm.createPaths()
			}
		}
	}
	// Close paths
	pm.closePaths()
}

func getIPVersion(ip net.IP) int {
	if ip.To4() != nil {
		return 4
	}
	return 6
}

func (pm *pathManager) advertiseAddresses() {
	pm.pconnMgr.mutex.Lock()
	defer pm.pconnMgr.mutex.Unlock()
	for _, locAddr := range pm.pconnMgr.localAddrs {
		_, sent := pm.advertisedLocAddrs[locAddr.String()]
		if !sent {
			version := getIPVersion(locAddr.IP)
			pm.sess.streamFramer.AddAddressForTransmission(uint8(version), locAddr)
			pm.advertisedLocAddrs[locAddr.String()] = true
		}
	}
}

func (pm *pathManager) createPath(locAddr net.UDPAddr, remAddr net.UDPAddr) error {
	// First check that the path does not exist yet
	pm.sess.pathsLock.Lock()
	defer pm.sess.pathsLock.Unlock()
	paths := pm.sess.paths
	for _, pth := range paths {
		locAddrPath := pth.conn.LocalAddr().String()
		remAddrPath := pth.conn.RemoteAddr().String()
		if locAddr.String() == locAddrPath && remAddr.String() == remAddrPath {
			// Path already exists, so don't create it again
			return nil
		}
	}
	// No matching path, so create it

	pth := &path{
		pathID: pm.nxtPathID,
		sess:   pm.sess,
		conn:   &conn{pconn: pm.pconnMgr.pconns[locAddr.String()], currentAddr: &remAddr},
	}

	localIP := locAddr.IP.String()
	var rtt time.Duration
	var bandwidth congestion.Bandwidth

	//only client can use this function
	if localIP == "10.0.0.1" {
		rtt = 1 * time.Millisecond
		bandwidth = 1
		bandwidth *= 1048576
	} else if localIP == "10.0.1.1" {
		rtt = 1 * time.Millisecond
		bandwidth = 20
		bandwidth *= 1048576
	} else {
		rtt = 0
		bandwidth = 0
	}
	pth.setupWithStatistics(pm.oliaSenders, rtt, bandwidth)
	pm.sess.paths[pm.nxtPathID] = pth
	pm.sess.openPaths = append(pm.sess.openPaths, pm.nxtPathID)

	if utils.Debug() {
		utils.Debugf("Created path %x on %s to %s, rtt initialized to %s", pm.nxtPathID, locAddr.String(), remAddr.String(), pth.rttStats.SmoothedRTT())
	}
	pm.nxtPathID += 2
	// Send a PING frame to get latency info about the new path and informing the
	// peer of its existence
	// Because we hold pathsLock, it is safe to send packet now
	return pm.sess.sendPing(pth)
}

func (pm *pathManager) createPaths() error {
	// if utils.Debug() {
	// 	utils.Debugf("Path manager tries to create paths")
	// }

	// XXX (QDC): don't let the server create paths for now
	if pm.sess.perspective == protocol.PerspectiveServer {
		pm.advertiseAddresses()
		return nil
	}
	// TODO (QDC): clearly not optimali
	pm.pconnMgr.mutex.Lock()
	defer pm.pconnMgr.mutex.Unlock()
	for _, locAddr := range pm.pconnMgr.localAddrs {
		version := getIPVersion(locAddr.IP)
		if version == 4 {
			for _, remAddr := range pm.remoteAddrs4 {
				err := pm.createPath(locAddr, remAddr)
				if err != nil {
					return err
				}
			}
		} else {
			for _, remAddr := range pm.remoteAddrs6 {
				err := pm.createPath(locAddr, remAddr)
				if err != nil {
					return err
				}
			}
		}
	}
	pm.sess.schedulePathsFrame()
	return nil
}

func parseIP(remoteAddr net.Addr) string {
	addr := remoteAddr.String()
	s := strings.Split(addr, ":")
	ip := s[0]

	return ip

}

func (pm *pathManager) createPathFromRemote(p *receivedPacket) (*path, error) {
	pm.sess.pathsLock.Lock()
	defer pm.sess.pathsLock.Unlock()
	localPconn := p.rcvPconn
	remoteAddr := p.remoteAddr
	pathID := p.publicHeader.PathID

	// Sanity check: pathID should not exist yet
	_, ko := pm.sess.paths[pathID]
	if ko {
		return nil, errors.New("trying to create already existing path")
	}

	// Sanity check: odd is client initiated, even for server initiated
	if pm.sess.perspective == protocol.PerspectiveClient && pathID%2 != 0 {
		return nil, errors.New("server tries to create odd pathID")
	}
	if pm.sess.perspective == protocol.PerspectiveServer && pathID%2 == 0 {
		return nil, errors.New("client tries to create even pathID")
	}

	remoteIP := parseIP(remoteAddr)

	var rtt time.Duration
	var bandwidth congestion.Bandwidth

	if remoteIP == "10.0.0.1" {
		rtt = 1 * time.Millisecond
		bandwidth = 1
		bandwidth *= 1048576
	} else if remoteIP == "10.0.1.1" {
		rtt = 1 * time.Millisecond
		bandwidth = 20
		bandwidth *= 1048576
	} else {
		rtt = 0
		bandwidth = 0

	}

	pth := &path{
		pathID: pathID,
		sess:   pm.sess,
		conn:   &conn{pconn: localPconn, currentAddr: remoteAddr},
	}
	pth.setupWithStatistics(pm.oliaSenders, rtt, bandwidth)
	//pth.setup(pm.oliaSenders)
	pm.sess.paths[pathID] = pth
	pm.sess.openPaths = append(pm.sess.openPaths, pathID)

	if utils.Debug() {
		utils.Debugf("Created remote path %x on %s to %s, rtt initialized to %s", pathID, localPconn.LocalAddr().String(), remoteAddr.String(), pth.rttStats.SmoothedRTT())
	}

	return pth, nil
}

func (pm *pathManager) createPathsFromRemotePathsFrame(frame *wire.PathsFrame, localPconn net.PacketConn) error {

	for i := 0; i < len(frame.PathIDs); i++ {
		pathID := frame.PathIDs[i]

		remoteIP := frame.RemoteAddrsIP[i]
		remotePort := frame.RemoteAddrsPort[i]

		port, err := strconv.Atoi(remotePort)
		if err != nil {
			return errors.New("error parsing port")
		}
		remoteAddr := &net.UDPAddr{
			IP:   net.ParseIP(remoteIP),
			Port: port,
		}

		// Sanity check: pathID should not exist yet
		_, ko := pm.sess.paths[pathID]
		if ko {
			//trying to create already existing path, continue to check next path
			continue
		}

		// Sanity check: odd is client initiated, even for server initiated
		if pm.sess.perspective == protocol.PerspectiveClient && pathID%2 != 0 {
			return errors.New("server tries to create odd pathID")
		}
		if pm.sess.perspective == protocol.PerspectiveServer && pathID%2 == 0 {
			return errors.New("client tries to create even pathID")
		}

		var rtt time.Duration
		var bandwidth congestion.Bandwidth

		if remoteIP == "10.0.0.1" {
			rtt = 1 * time.Millisecond
			bandwidth = 1
			bandwidth *= 1048576
		} else if remoteIP == "10.0.1.1" {
			rtt = 1 * time.Millisecond
			bandwidth = 20
			bandwidth *= 1048576
		} else {
			rtt = 0
			bandwidth = 0

		}

		pth := &path{
			pathID: pathID,
			sess:   pm.sess,
			conn:   &conn{pconn: localPconn, currentAddr: remoteAddr},
		}
		pth.setupWithStatistics(pm.oliaSenders, rtt, bandwidth)
		//pth.setup(pm.oliaSenders)
		pm.sess.paths[pathID] = pth
		pm.sess.openPaths = append(pm.sess.openPaths, pathID)

		if utils.Debug() {
			utils.Debugf("Based on PathsFrame: Created remote path %x on %s to %s, rtt initialized to %s", pathID, localPconn.LocalAddr().String(), remoteAddr.String(), pth.rttStats.SmoothedRTT())
		}

	}
	return nil

}

func (pm *pathManager) handleAddAddressFrame(f *wire.AddAddressFrame) error {
	switch f.IPVersion {
	case 4:
		pm.remoteAddrs4 = append(pm.remoteAddrs4, f.Addr)
	case 6:
		pm.remoteAddrs6 = append(pm.remoteAddrs6, f.Addr)
	default:
		return wire.ErrUnknownIPVersion
	}
	if pm.sess.createPaths {
		return pm.createPaths()
	}
	return nil
}

func (pm *pathManager) closePath(pthID protocol.PathID) error {
	pm.sess.pathsLock.RLock()
	defer pm.sess.pathsLock.RUnlock()

	pth, ok := pm.sess.paths[pthID]
	if !ok {
		// XXX (QDC) Unknown path, what should we do?
		return nil
	}

	if pth.open.Get() {
		pth.closeChan <- nil
	}

	return nil
}

func (pm *pathManager) closePaths() {
	pm.sess.pathsLock.RLock()
	paths := pm.sess.paths
	for _, pth := range paths {
		if pth.open.Get() {
			select {
			case pth.closeChan <- nil:
			default:
				// Don't remain stuck here!
			}
		}
	}
	pm.sess.pathsLock.RUnlock()
}
