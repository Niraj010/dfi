// This represents a peer in the network.
// the minimum that a peer requires to be "valid" is just an address.
// everything else can be discovered via the network.
// Just a bit of a wrapper for the client really, that contains most of the networking code, this mostly has the data and a few other things.

package libzif

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"net"
	"time"

	"github.com/hashicorp/yamux"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ed25519"

	"github.com/zif/zif/data"
	"github.com/zif/zif/dht"
	"github.com/zif/zif/proto"
	"github.com/zif/zif/util"

	"github.com/zif/zif/common"
)

type Peer struct {
	address dht.Address

	publicKey ed25519.PublicKey
	streams   proto.StreamManager

	limiter *util.PeerLimiter

	entry *proto.Entry

	// If this peer is acting as a seed for another
	seed    bool
	seedFor *proto.Entry
}

func (p *Peer) EAddress() common.Encodable {
	return &p.address
}

func (p *Peer) Address() *dht.Address {
	return &p.address
}

func (p *Peer) PublicKey() []byte {
	return p.publicKey
}

func (p *Peer) Streams() *proto.StreamManager {
	return &p.streams
}

func (p *Peer) Ping(timeOut time.Duration) (time.Duration, error) {
	type timeErr struct {
		t   time.Duration
		err error
	}

	session := p.streams.GetSession()

	if session == nil {
		return -1, errors.New("No session")
	}

	if session.IsClosed() {
		return -1, errors.New("Session closed")
	}

	timer := time.NewTimer(timeOut)

	ret := make(chan timeErr)

	go func() {
		t, err := session.Ping()
		ret <- timeErr{t, err}
	}()

	select {
	case ping := <-ret:
		return ping.t, ping.err

	case _ = <-timer.C:
		return -1, errors.New("Timeout")
	}
}

func (p *Peer) Announce(lp *LocalPeer) error {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return err
	}

	s, _ := p.Address().String()
	log.Debug("Sending announce to ", s)

	if lp.Entry.PublicAddress == "" {
		log.Debug("Local peer public address is nil, attempting to fetch")
		ip := external_ip()
		log.Debug("External IP is ", ip)
		lp.Entry.PublicAddress = ip
	}
	lp.SignEntry()

	stream, err := p.OpenStream()

	if err != nil {
		return err
	}

	defer stream.Close()

	err = stream.Announce(lp.Entry)

	return err
}

func (p *Peer) Connect(addr string, lp *LocalPeer) error {
	log.Debug("Connecting to ", addr)

	pair, err := p.streams.OpenTCP(addr, lp, lp.Entry)

	if err != nil {
		return err
	}

	p.publicKey = pair.Entry.PublicKey
	p.address = pair.Entry.Address

	p.limiter = &util.PeerLimiter{}
	p.limiter.Setup()

	encoded, _ := pair.Entry.Json()
	lp.DHT.Insert(dht.NewKeyValue(pair.Entry.Address, encoded))

	return nil
}

func (p *Peer) SetTCP(header proto.ConnHeader) {
	p.streams.SetConnection(header)

	p.publicKey = header.Entry.PublicKey
	p.address = header.Entry.Address

	p.limiter = &util.PeerLimiter{}
	p.limiter.Setup()
}

func (p *Peer) ConnectServer() (*yamux.Session, error) {
	return p.streams.ConnectServer()
}

func (p *Peer) ConnectClient(lp *LocalPeer) (*yamux.Session, error) {
	client, err := p.streams.ConnectClient()

	if err != nil {
		return client, err
	}

	go lp.ListenStream(p)

	return client, err
}

func (p *Peer) Session() *yamux.Session {
	return p.streams.GetSession()
}

func (p *Peer) Terminate() {
	p.streams.Close()
}

func (p *Peer) OpenStream() (*proto.Client, error) {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return nil, err
	}

	s, err := p.streams.OpenStream()
	return s, err
}

func (p *Peer) AddStream(conn net.Conn) {
	p.streams.AddStream(conn)
}

func (p *Peer) RemoveStream(conn net.Conn) {
	p.streams.RemoveStream(conn)
}

func (p *Peer) GetStream(conn net.Conn) *proto.Client {
	return p.streams.GetStream(conn)
}

func (p *Peer) CloseStreams() {
	p.streams.Close()
}

func (p *Peer) Entry() (*proto.Entry, error) {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return nil, err
	}

	if p.entry != nil {
		return p.entry, nil
	}

	s, _ := p.Address().String()
	kv, err := p.Query(s)

	if err != nil {
		return nil, err
	}

	entry, err := proto.JsonToEntry(kv.Value())

	if err != nil {
		return nil, err
	}

	log.WithField("addr", s).Info("Recieved")

	if !entry.Address.Equals(p.Address()) {
		return nil, errors.New("Failed to fetch entry")
	}

	p.entry = entry

	return p.entry, nil
}

func (p *Peer) Bootstrap(d *dht.DHT) error {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return err
	}

	initial, err := p.Entry()

	if err != nil {
		return err
	}

	dat, _ := initial.Json()

	d.Insert(dht.NewKeyValue(initial.Address, dat))

	stream, err := p.OpenStream()

	if err != nil {
		return err
	}

	defer stream.Close()

	return stream.Bootstrap(d, d.Address())
}

func (p *Peer) Query(address string) (*dht.KeyValue, error) {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return nil, err
	}

	log.WithField("target", address).Info("Querying")

	stream, err := p.OpenStream()

	if err != nil {
		return nil, err
	}

	defer stream.Close()

	entry, err := stream.Query(address)

	return entry, err
}

func (p *Peer) FindClosest(address string) (dht.Pairs, error) {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return nil, err
	}

	log.WithField("target", address).Info("Finding closest")

	stream, err := p.OpenStream()

	if err != nil {
		return nil, err
	}

	defer stream.Close()

	res, err := stream.FindClosest(address)

	return res, err
}

// asks a peer to query its database and return the results
func (p *Peer) Search(search string, page int) (*data.SearchResult, error) {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return nil, err
	}

	s, _ := p.Address().String()
	log.WithField("Query", s).Info("Searching")
	stream, err := p.OpenStream()

	if err != nil {
		return nil, err
	}

	defer stream.Close()

	posts, err := stream.Search(search, page)
	res := &data.SearchResult{
		Posts:  posts,
		Source: s,
	}

	if err != nil {
		return nil, err
	}

	return res, nil
}

func (p *Peer) Recent(page int) ([]*data.Post, error) {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return nil, err
	}

	stream, err := p.OpenStream()

	if err != nil {
		return nil, err
	}

	defer stream.Close()

	posts, err := stream.Recent(page)

	return posts, err

}

func (p *Peer) Popular(page int) ([]*data.Post, error) {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return nil, err
	}

	stream, err := p.OpenStream()

	if err != nil {
		return nil, err
	}

	defer stream.Close()

	posts, err := stream.Popular(page)

	return posts, err

}

func (p *Peer) Mirror(db *data.Database, onPiece chan int) error {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return err
	}

	pieces := make(chan *data.Piece, data.PieceSize)
	defer close(pieces)
	defer close(onPiece)

	go db.InsertPieces(pieces, true)

	s, _ := p.Address().String()
	log.WithField("peer", s).Info("Mirroring")

	stream, err := p.OpenStream()

	if err != nil {
		return err
	}

	defer stream.Close()

	var entry *proto.Entry
	if p.seed {
		entry = p.seedFor
	} else {
		entry, err = p.Entry()
	}

	if err != nil {
		return err
	}

	mcol, err := stream.Collection(entry.Address, entry.PublicKey)

	if err != nil {
		return err
	}

	collection := data.Collection{HashList: mcol.HashList}
	es, _ := entry.Address.String()
	collection.Save(fmt.Sprintf("./data/%s/collection.dat", es))

	if err != nil {
		return err
	}

	if int(db.PostCount()) == p.entry.PostCount {
		return nil
	}

	currentStore := int(math.Ceil(float64(db.PostCount()) / float64(data.PieceSize)))

	since := 0
	if currentStore != 0 {
		since = currentStore - 1
	}

	log.WithField("Size", mcol.Size).Info("Downloading collection")
	piece_stream := stream.Pieces(entry.Address, since, mcol.Size)

	i := 0
	for piece := range piece_stream {
		log.Info(len(mcol.HashList))
		hash := piece.Hash()

		if !bytes.Equal(mcol.HashList[32*i:32*i+32], hash) {
			return errors.New("Piece hash mismatch")
		}

		onPiece <- i

		if len(pieces) == 100 {
			log.Info("Piece buffer full, io is blocking")
		}
		pieces <- piece

		i++
	}

	log.Info("Mirror complete")

	p.RequestAddPeer(s)

	return err
}

func (p *Peer) RequestAddPeer(addr string) error {
	_, err := p.Ping(time.Second * 10)
	if err != nil {
		return err
	}

	stream, err := p.OpenStream()

	if err != nil {
		return err
	}

	defer stream.Close()

	return stream.RequestAddPeer(addr)
}
