package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nictuku/dht"
	"github.com/zeebo/bencode"

	"github.com/rakoo/rakoshare/pkg/id"
)

const (
	MAX_NUM_PEERS    = 60
	TARGET_NUM_PEERS = 15
)

// BitTorrent message types. Sources:
// http://bittorrent.org/beps/bep_0003.html
// http://wiki.theory.org/BitTorrentSpecification
const (
	CHOKE = iota
	UNCHOKE
	INTERESTED
	NOT_INTERESTED
	HAVE
	BITFIELD
	REQUEST
	PIECE
	CANCEL
	PORT      // Not implemented. For DHT support.
	EXTENSION = 20
)

const (
	EXTENSION_HANDSHAKE = iota
)

// Should be overriden by flag. Not thread safe.
var gateway string
var fileDir string
var useDHT bool
var trackerLessMode bool

func init() {
	flag.StringVar(&fileDir, "fileDir", ".", "path to directory where files are stored")
	// If the port is 0, picks up a random port - but the DHT will keep
	// running on port 0 because ListenUDP doesn't do that.
	// Don't use port 6881 which blacklisted by some trackers.
	flag.BoolVar(&useDHT, "useDHT", false, "Use DHT to get peers.")
	flag.BoolVar(&trackerLessMode, "trackerLessMode", true, "Do not get peers from the tracker. Good for "+
		"testing the DHT mode.")
	flag.StringVar(&gateway, "gateway", "", "IP Address of gateway.")
}

func peerId() string {
	sid := "-tt" + strconv.Itoa(os.Getpid()) + "_" + strconv.FormatInt(rand.Int63(), 10)
	return sid[0:20]
}

var kBitTorrentHeader = []byte{'\x13', 'B', 'i', 't', 'T', 'o', 'r',
	'r', 'e', 'n', 't', ' ', 'p', 'r', 'o', 't', 'o', 'c', 'o', 'l'}

type ActivePiece struct {
	downloaderCount []int // -1 means piece is already downloaded
	pieceLength     int
}

func (a *ActivePiece) chooseBlockToDownload(endgame bool) (index int) {
	if endgame {
		return a.chooseBlockToDownloadEndgame()
	}
	return a.chooseBlockToDownloadNormal()
}

func (a *ActivePiece) chooseBlockToDownloadNormal() (index int) {
	for i, v := range a.downloaderCount {
		if v == 0 {
			a.downloaderCount[i]++
			return i
		}
	}
	return -1
}

func (a *ActivePiece) chooseBlockToDownloadEndgame() (index int) {
	index, minCount := -1, -1
	for i, v := range a.downloaderCount {
		if v >= 0 && (minCount == -1 || minCount > v) {
			index, minCount = i, v
		}
	}
	if index > -1 {
		a.downloaderCount[index]++
	}
	return
}

func (a *ActivePiece) recordBlock(index int) (requestCount int) {
	requestCount = a.downloaderCount[index]
	a.downloaderCount[index] = -1
	return
}

func (a *ActivePiece) isComplete() bool {
	for _, v := range a.downloaderCount {
		if v != -1 {
			return false
		}
	}
	return true
}

type TorrentSessionI interface {
	NewMetaInfo() chan *MetaInfo

	IsEmpty() bool
	Quit() error
	Matches(ih string) bool
	AcceptNewPeer(btc *btConn)
	DoTorrent()
	hintNewPeer(peer string)
}

type TorrentSession struct {
	m                 *MetaInfo
	si                *SessionInfo
	ti                *TrackerResponse
	torrentHeader     []byte
	fileStore         FileStore
	trackerReportChan chan ClientStatusReport
	trackerInfoChan   chan *TrackerResponse
	peers             map[string]*peerState
	peerMessageChan   chan peerMessage
	pieceSet          *Bitset // The pieces we have
	totalPieces       int
	totalSize         int64
	lastPieceLength   int
	goodPieces        int
	activePieces      map[int]*ActivePiece
	heartbeat         chan bool
	dht               *dht.DHT
	quit              chan bool
	trackerLessMode   bool

	miChan chan *MetaInfo
	Id     id.Id
}

func NewTorrentSession(shareId id.Id, torrent string, listenPort int) (ts *TorrentSession, err error) {
	t := &TorrentSession{
		Id:              shareId,
		peers:           make(map[string]*peerState),
		peerMessageChan: make(chan peerMessage),
		activePieces:    make(map[int]*ActivePiece),
		quit:            make(chan bool),
		miChan:          make(chan *MetaInfo),
	}

	if useDHT {
		// TODO: UPnP UDP port mapping.
		cfg := dht.NewConfig()
		cfg.Port = listenPort
		cfg.NumTargetPeers = TARGET_NUM_PEERS
		if t.dht, err = dht.New(cfg); err != nil {
			log.Println("DHT node creation error", err)
			return
		}
		go t.dht.Run()
	}

	fromMagnet := strings.HasPrefix(torrent, "magnet:")
	t.m, err = getMetaInfo(torrent)
	if err != nil {
		return
	}

	go t.StartPex()

	t.si = &SessionInfo{
		PeerId:      peerId(),
		Port:        listenPort,
		UseDHT:      useDHT,
		FromMagnet:  fromMagnet,
		HaveTorrent: false,
		ME:          &MetaDataExchange{},
		OurExtensions: map[int]string{
			1: "ut_metadata",
			2: "ut_pex",
		},
	}

	if !t.si.FromMagnet {
		err = t.load()
	}
	return t, err
}

func (t *TorrentSession) NewMetaInfo() chan *MetaInfo {
	return t.miChan
}

func (t *TorrentSession) reload(info []byte) error {
	err := bencode.NewDecoder(bytes.NewReader(info)).Decode(&t.m.Info)
	if err != nil {
		log.Println("Error when reloading torrent: ", err)
		return err
	}

	t.miChan <- t.m
	return t.load()
}

func (t *TorrentSession) load() error {
	var err error

	log.Printf("Tracker: %v, Comment: %v, InfoHash: %x, Encoding: %v, Private: %v",
		t.m.AnnounceList, t.m.Comment, t.m.InfoHash, t.m.Encoding, t.m.Info.Private)
	if e := t.m.Encoding; e != "" && e != "UTF-8" {
		log.Printf("Invalid encoding, couldn't load: %s\n", e)
		return errors.New("Invalid encoding: " + e)
	}

	if t.m.Announce == "" {
		t.trackerLessMode = true
	} else {
		t.trackerLessMode = trackerLessMode
	}

	t.fileStore, t.totalSize, err = NewFileStore(t.m.Info, fileDir)
	if err != nil {
		log.Fatal("Couldn't create filestore: ", err)
	}
	t.lastPieceLength = int(t.totalSize % t.m.Info.PieceLength)
	if t.lastPieceLength == 0 { // last piece is a full piece
		t.lastPieceLength = int(t.m.Info.PieceLength)
	}

	start := time.Now()
	good, bad, pieceSet, err := checkPieces(t.fileStore, t.totalSize, t.m)
	if err != nil {
		return errors.New(fmt.Sprintf("Error when checking pieces: %s", err))
	}
	end := time.Now()
	log.Printf("Computed missing pieces (%.2f seconds)", end.Sub(start).Seconds())
	if err != nil {
		return err
	}
	t.pieceSet = pieceSet
	t.totalPieces = good + bad
	t.goodPieces = good
	log.Println("Good pieces:", good, "Bad pieces:", bad)

	left := int64(bad) * int64(t.m.Info.PieceLength)
	if !t.pieceSet.IsSet(t.totalPieces - 1) {
		left = left - t.m.Info.PieceLength + int64(t.lastPieceLength)
	}

	if left == 0 {
		err := t.fileStore.Cleanup()
		if err != nil {
			log.Println("Couldn't cleanup correctly: ", err)
		}
	}

	t.si.HaveTorrent = true
	return nil
}

func (t *TorrentSession) IsEmpty() bool {
	return false
}

func (t *TorrentSession) fetchTrackerInfo(event string) {
	m, si := t.m, t.si
	log.Println("Stats: Uploaded", si.Uploaded, "Downloaded", si.Downloaded, "Left", si.Left)
	t.trackerReportChan <- ClientStatusReport{
		event, m.InfoHash, si.PeerId, si.Port, si.Uploaded, si.Downloaded, si.Left}
}

func (ts *TorrentSession) Header() (header []byte) {
	if ts.torrentHeader != nil {
		return ts.torrentHeader
	}

	header = make([]byte, 68)
	copy(header, kBitTorrentHeader[0:])
	if ts.si.UseDHT {
		header[27] = header[27] | 0x01
	}
	// Support Extension Protocol (BEP-0010)
	header[25] |= 0x10

	copy(header[28:48], []byte(ts.m.InfoHash))
	copy(header[48:68], []byte(ts.si.PeerId))

	ts.torrentHeader = header

	return
}

// Try to connect if the peer is not already in our peers
func (ts *TorrentSession) hintNewPeer(peer string) {
	if _, ok := ts.peers[peer]; !ok {
		go ts.connectToPeer(peer)
	}
}

func (ts *TorrentSession) connectToPeer(peer string) {
	conn, err := NewTCPConn([]byte(ts.Id.Psk[:]), peer)
	if err != nil {
		log.Println("Failed to connect to", peer, err)
		return
	}

	_, err = conn.Write(ts.Header())
	if err != nil {
		log.Println("Failed to send header to", peer, err)
		return
	}

	theirheader, err := readHeader(conn)
	if err != nil {
		// log.Printf("Failed to read header from %s: %s", peer, err)
		return
	}

	peersInfoHash := string(theirheader[8:28])
	id := string(theirheader[28:48])

	// If it's us, we don't need to continue
	if id == ts.si.PeerId {
		conn.Close()
		return
	}

	btconn := &btConn{
		header:   theirheader,
		infohash: peersInfoHash,
		id:       id,
		conn:     conn,
	}
	// log.Println("Connected to", peer)
	ts.AddPeer(btconn)
}

func (t *TorrentSession) AcceptNewPeer(btconn *btConn) {
	_, err := btconn.conn.Write(t.Header())
	if err != nil {
		return
	}
	t.AddPeer(btconn)
}

func (t *TorrentSession) AddPeer(btconn *btConn) {
	for _, p := range t.peers {
		if p.id == btconn.id {
			return
		}
	}

	theirheader := btconn.header

	peer := btconn.conn.RemoteAddr().String()
	// log.Println("Adding peer", peer)
	if len(t.peers) >= MAX_NUM_PEERS {
		log.Println("We have enough peers. Rejecting additional peer", peer)
		btconn.conn.Close()
		return
	}
	ps := NewPeerState(btconn.conn)
	ps.address = peer
	ps.id = btconn.id
	if t.si.UseDHT {
		// If 128, then it supports DHT.
		if int(theirheader[7])&0x01 == 0x01 {
			// It's OK if we know this node already. The DHT engine will
			// ignore it accordingly.
			go t.dht.AddNode(ps.address)
		}
	}

	t.peers[peer] = ps

	if int(theirheader[5])&0x10 == 0x10 {
		ps.SendExtensions(t.si.OurExtensions, int64(len(t.m.RawInfo())))

		if t.si.HaveTorrent {
			ps.SendBitfield(t.pieceSet)
		}
	} else {
		ps.SendBitfield(t.pieceSet)
	}

	if t.si.HaveTorrent {
		// By default, a peer has no pieces. If it has pieces, it should send
		// a BITFIELD message as a first message
		ps.have = NewBitset(t.totalPieces)
	}

	// Note that we need to launch these at the end of initialisation, so
	// we are sure that the message we buffered previously will be the
	// first to be sent.
	go ps.peerWriter(t.peerMessageChan)
	go ps.peerReader(t.peerMessageChan)
}

func (t *TorrentSession) ClosePeer(peer *peerState) {
	if t.si.ME != nil && !t.si.ME.Transferring {
		t.si.ME.Transferring = false
	}

	_ = t.removeRequests(peer)
	peer.Close()
	delete(t.peers, peer.address)
}

func (t *TorrentSession) deadlockDetector(quit chan struct{}) {
	lastHeartbeat := time.Now()

deadlockLoop:
	for {
		select {
		case <-quit:
			break deadlockLoop
		case <-t.heartbeat:
			lastHeartbeat = time.Now()
		case <-time.After(15 * time.Second):
			age := time.Now().Sub(lastHeartbeat)
			log.Println("Starvation or deadlock of main thread detected. Look in the stack dump for what DoTorrent() is currently doing.")
			log.Println("Last heartbeat", age.Seconds(), "seconds ago")
			panic("Killed by deadlock detector")
		}
	}
}

func (t *TorrentSession) Quit() (err error) {
	t.quit <- true
	for _, peer := range t.peers {
		t.ClosePeer(peer)
	}
	if t.dht != nil {
		t.dht.Stop()
	}
	return nil
}

func (t *TorrentSession) DoTorrent() {
	t.heartbeat = make(chan bool, 1)
	quitDeadlock := make(chan struct{})
	go t.deadlockDetector(quitDeadlock)

	log.Println("[CURRENT] Start")

	rechokeChan := time.Tick(1 * time.Second)
	verboseChan := time.Tick(10 * time.Minute)
	keepAliveChan := time.Tick(60 * time.Second)
	var retrackerChan <-chan time.Time
	if !t.trackerLessMode {
		// Start out polling tracker every 20 seconds until we get a response.
		// Maybe be exponential backoff here?
		retrackerChan = time.Tick(20 * time.Second)
		t.trackerInfoChan = make(chan *TrackerResponse)
		t.trackerReportChan = make(chan ClientStatusReport)
		startTrackerClient(t.m.Announce, t.m.AnnounceList, t.trackerInfoChan, t.trackerReportChan)
	}

	if t.si.UseDHT {
		t.dht.PeersRequest(t.m.InfoHash, true)
	}

	if !t.trackerLessMode && t.si.HaveTorrent {
		t.fetchTrackerInfo("started")
	}

	for {
		var peersRequestResults chan map[dht.InfoHash][]string
		peersRequestResults = nil
		if t.dht != nil {
			peersRequestResults = t.dht.PeersRequestResults
		}
		select {
		case <-retrackerChan:
			if !t.trackerLessMode {
				t.fetchTrackerInfo("")
			}
		case dhtInfoHashPeers := <-peersRequestResults:
			newPeerCount := 0
			// key = infoHash. The torrent client currently only
			// supports one download at a time, so let's assume
			// it's the case.
			for _, peers := range dhtInfoHashPeers {
				for _, peer := range peers {
					peer = dht.DecodePeerAddress(peer)
					if _, ok := t.peers[peer]; !ok {
						newPeerCount++
						if t.si.HaveTorrent {
							go t.connectToPeer(peer)
						}
					}
				}
			}
			// log.Println("Contacting", newPeerCount, "new peers (thanks DHT!)")
		case ti := <-t.trackerInfoChan:
			t.ti = ti
			log.Println("Torrent has", t.ti.Complete, "seeders and", t.ti.Incomplete, "leachers.")
			if !t.trackerLessMode {
				newPeerCount := 0
				for _, peer := range ti.Peers {
					if _, ok := t.peers[peer]; !ok {
						newPeerCount++
						go t.connectToPeer(peer)
					}
				}
				for _, peer6 := range ti.Peers6 {
					if _, ok := t.peers[peer6]; !ok {
						newPeerCount++
						go t.connectToPeer(peer6)
					}
				}
				log.Println("Contacting", newPeerCount, "new peers")
				interval := t.ti.Interval
				if interval < 120 {
					interval = 120
				} else if interval > 24*3600 {
					interval = 24 * 3600
				}
				log.Println("..checking again in", interval, "seconds.")
				retrackerChan = time.Tick(interval * time.Second)
				log.Println("Contacting", newPeerCount, "new peers")
			}
			interval := t.ti.Interval
			if interval < 120 {
				interval = 120
			} else if interval > 24*3600 {
				interval = 24 * 3600
			}
			log.Println("..checking again in", interval.String())
			retrackerChan = time.Tick(interval * time.Second)

		case pm := <-t.peerMessageChan:
			peer, message := pm.peer, pm.message
			peer.lastReadTime = time.Now()
			err2 := t.DoMessage(peer, message)
			if err2 != nil {
				if err2 != io.EOF {
					log.Println("Closing peer", peer.address, "because", err2)
				}
				t.ClosePeer(peer)
			}
		case <-rechokeChan:
			// TODO: recalculate who to choke / unchoke
			t.heartbeat <- true
			if len(t.peers) < TARGET_NUM_PEERS && t.goodPieces < t.totalPieces {
				if t.si.UseDHT {
					go t.dht.PeersRequest(t.m.InfoHash, true)
				}
				if !t.trackerLessMode {
					if t.ti == nil || t.ti.Complete > 100 {
						t.fetchTrackerInfo("")
					}
				}
			}
		case <-verboseChan:
			ratio := float64(0.0)
			if t.si.Downloaded > 0 {
				ratio = float64(t.si.Uploaded) / float64(t.si.Downloaded)
			}
			log.Printf("[CURRENT] Peers: %d, good/total: %d/%d, ratio: %f\n",
				len(t.peers), t.goodPieces, t.totalPieces, ratio)
		case <-keepAliveChan:
			now := time.Now()
			for _, peer := range t.peers {
				if peer.lastReadTime.Second() != 0 && now.Sub(peer.lastReadTime) > 3*time.Minute {
					// log.Println("Closing peer", peer.address, "because timed out.")
					t.ClosePeer(peer)
					continue
				}
				err2 := t.doCheckRequests(peer)
				if err2 != nil {
					if err2 != io.EOF {
						log.Println("Closing peer", peer.address, "because", err2)
					}
					t.ClosePeer(peer)
					continue
				}
				peer.keepAlive(now)
			}

		case <-t.quit:
			log.Println("Quitting torrent session")
			quitDeadlock <- struct{}{}
			return
		}
	}

}

func (t *TorrentSession) RequestBlock(p *peerState) (err error) {
	for k, _ := range t.activePieces {
		if p.have.IsSet(k) {
			err = t.RequestBlock2(p, k, false)
			if err != io.EOF {
				return
			}
		}
	}
	// No active pieces. (Or no suitable active pieces.) Pick one
	piece := t.ChoosePiece(p)
	if piece < 0 {
		// No unclaimed pieces. See if we can double-up on an active piece
		for k, _ := range t.activePieces {
			if p.have.IsSet(k) {
				err = t.RequestBlock2(p, k, true)
				if err != io.EOF {
					return
				}
			}
		}
	}
	if piece >= 0 {
		pieceLength := int(t.m.Info.PieceLength)
		if piece == t.totalPieces-1 {
			pieceLength = t.lastPieceLength
		}
		pieceCount := (pieceLength + STANDARD_BLOCK_LENGTH - 1) / STANDARD_BLOCK_LENGTH
		t.activePieces[piece] = &ActivePiece{make([]int, pieceCount), pieceLength}
		return t.RequestBlock2(p, piece, false)
	} else {
		p.SetInterested(false)
	}
	return
}

func (t *TorrentSession) ChoosePiece(p *peerState) (piece int) {
	n := t.totalPieces
	start := rand.Intn(n)
	piece = t.checkRange(p, start, n)
	if piece == -1 {
		piece = t.checkRange(p, 0, start)
	}
	return
}

func (t *TorrentSession) checkRange(p *peerState, start, end int) (piece int) {
	for i := start; i < end; i++ {
		if !t.pieceSet.IsSet(i) && p.have.IsSet(i) {
			if _, ok := t.activePieces[i]; !ok {
				return i
			}
		}
	}
	return -1
}

func (t *TorrentSession) RequestBlock2(p *peerState, piece int, endGame bool) (err error) {
	v := t.activePieces[piece]
	block := v.chooseBlockToDownload(endGame)
	if block >= 0 {
		t.requestBlockImp(p, piece, block, true)
	} else {
		return io.EOF
	}
	return
}

// Request or cancel a block
func (t *TorrentSession) requestBlockImp(p *peerState, piece int, block int, request bool) {
	begin := block * STANDARD_BLOCK_LENGTH
	req := make([]byte, 13)
	opcode := byte(REQUEST)
	if !request {
		opcode = byte(CANCEL)
	}
	length := STANDARD_BLOCK_LENGTH
	if piece == t.totalPieces-1 {
		left := t.lastPieceLength - begin
		if left < length {
			length = left
		}
	}
	// log.Println("Requesting block", piece, ".", block, length, request)
	req[0] = opcode
	uint32ToBytes(req[1:5], uint32(piece))
	uint32ToBytes(req[5:9], uint32(begin))
	uint32ToBytes(req[9:13], uint32(length))
	requestIndex := (uint64(piece) << 32) | uint64(begin)
	if !request {
		delete(p.our_requests, requestIndex)
	} else {
		p.our_requests[requestIndex] = time.Now()
	}
	p.sendMessage(req)
	return
}

func (t *TorrentSession) RecordBlock(p *peerState, piece, begin, length uint32) (err error) {
	block := begin / STANDARD_BLOCK_LENGTH
	// log.Println("Received block", piece, ".", block)
	requestIndex := (uint64(piece) << 32) | uint64(begin)
	delete(p.our_requests, requestIndex)
	v, ok := t.activePieces[int(piece)]
	if ok {
		requestCount := v.recordBlock(int(block))
		if requestCount > 1 {
			// Someone else has also requested this, so send cancel notices
			for _, peer := range t.peers {
				if p != peer {
					if _, ok := peer.our_requests[requestIndex]; ok {
						t.requestBlockImp(peer, int(piece), int(block), false)
						requestCount--
					}
				}
			}
		}
		t.si.Downloaded += int64(length)
		if v.isComplete() {
			delete(t.activePieces, int(piece))
			ok, err = checkPiece(t.fileStore, t.totalSize, t.m, int(piece))
			if !ok || err != nil {
				log.Println("Closing peer that sent a bad piece", piece, p.id, err)
				p.Close()
				return
			}
			t.si.Left -= int64(v.pieceLength)
			t.pieceSet.Set(int(piece))
			t.goodPieces++
			log.Println("Have", t.goodPieces, "of", t.totalPieces, "pieces.")
			if t.goodPieces == t.totalPieces {
				if !t.trackerLessMode {
					t.fetchTrackerInfo("completed")
				}
				err := t.fileStore.Cleanup()
				if err != nil {
					log.Println("Couldn't cleanup correctly: ", err)
				}

				// TODO: Drop connections to all seeders.
			}
			for _, p := range t.peers {
				if p.have != nil {
					if p.have.IsSet(int(piece)) {
						// We don't do anything special. We rely on the caller
						// to decide if this peer is still interesting.
					} else {
						// log.Println("...telling ", p)
						haveMsg := make([]byte, 5)
						haveMsg[0] = HAVE
						uint32ToBytes(haveMsg[1:5], uint32(piece))
						p.sendMessage(haveMsg)
					}
				}
			}
		}
	} else {
		log.Println("Received a block we already have.", piece, block, p.address)
	}
	return
}

func (t *TorrentSession) doChoke(p *peerState) (err error) {
	p.peer_choking = true
	err = t.removeRequests(p)
	return
}

func (t *TorrentSession) removeRequests(p *peerState) (err error) {
	for k, _ := range p.our_requests {
		piece := int(k >> 32)
		begin := int(k & 0xffffffff)
		block := begin / STANDARD_BLOCK_LENGTH
		// log.Println("Forgetting we requested block ", piece, ".", block)
		t.removeRequest(piece, block)
	}
	p.our_requests = make(map[uint64]time.Time, MAX_OUR_REQUESTS)
	return
}

func (t *TorrentSession) removeRequest(piece, block int) {
	v, ok := t.activePieces[piece]
	if ok && v.downloaderCount[block] > 0 {
		v.downloaderCount[block]--
	}
}

func (t *TorrentSession) doCheckRequests(p *peerState) (err error) {
	now := time.Now()
	for k, v := range p.our_requests {
		if now.Sub(v).Seconds() > 30 {
			piece := int(k >> 32)
			block := int(k&0xffffffff) / STANDARD_BLOCK_LENGTH
			// log.Println("timing out request of", piece, ".", block)
			t.removeRequest(piece, block)
		}
	}
	return
}

func (t *TorrentSession) DoMessage(p *peerState, message []byte) (err error) {
	if message == nil {
		return io.EOF // The reader or writer goroutine has exited
	}
	if len(message) == 0 { // keep alive
		return
	}

	if t.si.HaveTorrent {
		err = t.generalMessage(message, p)
	} else {
		err = t.extensionMessage(message, p)
	}
	return
}

func (t *TorrentSession) extensionMessage(message []byte, p *peerState) (err error) {
	switch message[0] {
	case CHOKE:
		p.peer_choking = true
	case UNCHOKE:
		p.peer_choking = false
	case BITFIELD:
		p.SetChoke(false) // TODO: better choke policy

		p.temporaryBitfield = make([]byte, len(message[1:]))
		copy(p.temporaryBitfield, message[1:])
		p.can_receive_bitfield = false
	case EXTENSION:
		err := t.DoExtension(message[1:], p)
		if err != nil {
			log.Printf("Failed extensions for %s: %s\n", p.address, err)
		}
	}
	return
}

func (t *TorrentSession) generalMessage(message []byte, p *peerState) (err error) {
	messageId := message[0]

	switch messageId {
	case CHOKE:
		// log.Println("choke", p.address)
		if len(message) != 1 {
			return errors.New("Unexpected length")
		}
		err = t.doChoke(p)
	case UNCHOKE:
		// log.Println("unchoke", p.address)
		if len(message) != 1 {
			return errors.New("Unexpected length")
		}
		p.peer_choking = false
		for i := 0; i < MAX_OUR_REQUESTS; i++ {
			err = t.RequestBlock(p)
			if err != nil {
				return
			}
		}
	case INTERESTED:
		// log.Println("interested", p)
		if len(message) != 1 {
			return errors.New("Unexpected length")
		}
		p.peer_interested = true

		// TODO: Consider better unchoking policy (this is needed for
		// clients like Transmission who don't send a BITFIELD so we have to
		// unchoke them at this moment)
		p.SetChoke(false)
	case NOT_INTERESTED:
		// log.Println("not interested", p)
		if len(message) != 1 {
			return errors.New("Unexpected length")
		}
		p.peer_interested = false
	case HAVE:
		if len(message) != 5 {
			return errors.New("Unexpected length")
		}
		n := bytesToUint32(message[1:])
		if n < uint32(p.have.n) {
			p.have.Set(int(n))
			if !p.am_interested && !t.pieceSet.IsSet(int(n)) {
				p.SetInterested(true)
			}
		} else {
			return errors.New("have index is out of range.")
		}
	case BITFIELD:
		// log.Println("bitfield", p.address)
		if !p.can_receive_bitfield {
			return errors.New("Late bitfield operation")
		}
		p.SetChoke(false) // TODO: better choke policy

		p.have = NewBitsetFromBytes(t.totalPieces, message[1:])
		if p.have == nil {
			return errors.New("Invalid bitfield data.")
		}
		t.checkInteresting(p)
		p.can_receive_bitfield = false

		if p.peer_choking == false {
			for i := 0; i < MAX_OUR_REQUESTS; i++ {
				err = t.RequestBlock(p)
				if err != nil {
					return
				}
			}
		}
	case REQUEST:
		// log.Println("request", p.address)
		if len(message) != 13 {
			return errors.New("Unexpected message length")
		}
		index := bytesToUint32(message[1:5])
		begin := bytesToUint32(message[5:9])
		length := bytesToUint32(message[9:13])
		if index >= uint32(p.have.n) {
			return errors.New("piece out of range.")
		}
		if !t.pieceSet.IsSet(int(index)) {
			return errors.New("we don't have that piece.")
		}
		if int64(begin) >= t.m.Info.PieceLength {
			return errors.New("begin out of range.")
		}
		if int64(begin)+int64(length) > t.m.Info.PieceLength {
			return errors.New("begin + length out of range.")
		}
		// TODO: Asynchronous
		// p.AddRequest(index, begin, length)
		return t.sendRequest(p, index, begin, length)
	case PIECE:
		// piece
		if len(message) < 9 {
			return errors.New("unexpected message length")
		}
		index := bytesToUint32(message[1:5])
		begin := bytesToUint32(message[5:9])
		length := len(message) - 9
		if index >= uint32(p.have.n) {
			return errors.New("piece out of range.")
		}
		if t.pieceSet.IsSet(int(index)) {
			// We already have that piece, keep going
			break
		}
		if int64(begin) >= t.m.Info.PieceLength {
			return errors.New("begin out of range.")
		}
		if int64(begin)+int64(length) > t.m.Info.PieceLength {
			return errors.New("begin + length out of range.")
		}
		if length > 128*1024 {
			return errors.New("Block length too large.")
		}
		globalOffset := int64(index)*t.m.Info.PieceLength + int64(begin)
		_, err = t.fileStore.WriteAt(message[9:], globalOffset)
		if err != nil {
			return err
		}
		t.RecordBlock(p, index, begin, uint32(length))
		err = t.RequestBlock(p)
	case CANCEL:
		// log.Println("cancel")
		if len(message) != 13 {
			return errors.New("Unexpected message length")
		}
		index := bytesToUint32(message[1:5])
		begin := bytesToUint32(message[5:9])
		length := bytesToUint32(message[9:13])
		if index >= uint32(p.have.n) {
			return errors.New("piece out of range.")
		}
		if !t.pieceSet.IsSet(int(index)) {
			return errors.New("we don't have that piece.")
		}
		if int64(begin) >= t.m.Info.PieceLength {
			return errors.New("begin out of range.")
		}
		if int64(begin)+int64(length) > t.m.Info.PieceLength {
			return errors.New("begin + length out of range.")
		}
		if length != STANDARD_BLOCK_LENGTH {
			return errors.New("Unexpected block length.")
		}
		p.CancelRequest(index, begin, length)
	case PORT:
		// TODO: Implement this message.
		// We see peers sending us 16K byte messages here, so
		// it seems that we don't understand what this is.
		if len(message) != 3 {
			return fmt.Errorf("Unexpected length for port message: %d", len(message))
		}
		go t.dht.AddNode(p.address)
	case EXTENSION:
		err := t.DoExtension(message[1:], p)
		if err != nil {
			log.Printf("Failed extensions for %s: %s\n", p.address, err)
		}

	default:
		return errors.New(fmt.Sprintf("Uknown message id: %d\n", messageId))
	}

	return
}

type ExtensionHandshake struct {
	M            map[string]int `bencode:"m"`
	V            string         `bencode:"v"`
	P            uint16         `bencode:"p,omitempty"`
	Yourip       string         `bencode:"yourip,omitempty"`
	Ipv6         string         `bencode:"ipv6,omitempty"`
	Ipv4         string         `bencode:"ipv4,omitempty"`
	Reqq         uint16         `bencode:"reqq,omitempty"`
	MetadataSize int64          `bencode:"metadata_size,omitempty"`
}

func (t *TorrentSession) DoExtension(msg []byte, p *peerState) (err error) {

	var h ExtensionHandshake
	if msg[0] == EXTENSION_HANDSHAKE {
		err = bencode.NewDecoder(bytes.NewReader(msg[1:])).Decode(&h)
		if err != nil {
			log.Println("Error when unmarshaling extension handshake")
			return err
		}

		p.theirExtensions = make(map[string]int)
		for name, code := range h.M {
			p.theirExtensions[name] = code
		}

		if t.si.HaveTorrent || t.si.ME != nil && t.si.ME.Transferring {
			return
		}

		// Fill metadata info
		if h.MetadataSize == 0 {
			log.Printf("Missing metadata_size argument, this is invalid.")
			return
		}

		nPieces := h.MetadataSize/METADATA_PIECE_SIZE + 1
		t.si.ME.Pieces = make([][]byte, nPieces)

		if _, ok := p.theirExtensions["ut_metadata"]; ok {
			t.si.ME.Transferring = true
			p.sendMetadataRequest(0)
		}

	} else if ext, ok := t.si.OurExtensions[int(msg[0])]; ok {
		switch ext {
		case "ut_metadata":
			t.DoMetadata(msg[1:], p)
		case "ut_pex":
			t.DoPex(msg[1:], p)
		default:
			log.Println("Unknown extension: ", ext)
		}
	} else {
		log.Println("Unknown extension: ", int(msg[0]))
	}

	return nil
}

func (t *TorrentSession) sendRequest(peer *peerState, index, begin, length uint32) (err error) {
	if !peer.am_choking {
		// log.Println("Sending block", index, begin, length)
		buf := make([]byte, length+9)
		buf[0] = PIECE
		uint32ToBytes(buf[1:5], index)
		uint32ToBytes(buf[5:9], begin)
		_, err = t.fileStore.ReadAt(buf[9:],
			int64(index)*t.m.Info.PieceLength+int64(begin))
		if err != nil {
			return
		}
		peer.sendMessage(buf)
		t.si.Uploaded += int64(length)
	}
	return
}

func (t *TorrentSession) checkInteresting(p *peerState) {
	p.SetInterested(t.isInteresting(p))
}

func (t *TorrentSession) isInteresting(p *peerState) bool {
	for i := 0; i < t.totalPieces; i++ {
		if !t.pieceSet.IsSet(i) && p.have.IsSet(i) {
			return true
		}
	}
	return false
}

func (t *TorrentSession) Matches(ih string) bool {
	return t.m.InfoHash == ih
}
