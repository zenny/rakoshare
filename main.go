package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime/pprof"

	"github.com/rakoo/rakoshare/pkg/id"
	"github.com/rakoo/rakoshare/pkg/sharesession"

	"github.com/codegangsta/cli"
)

var (
	cpuprofile = flag.String("cpuprofile", "", "If not empty, collects CPU profile samples and writes the profile to the given file before the program exits")
	memprofile = flag.String("memprofile", "", "If not empty, writes memory heap allocations to the given file before the program exits")
	generate   = flag.Bool("gen", false, "If true, generate a 3-tuple of ids")
)

var torrent string

func main() {
	flag.Parse()

	if *cpuprofile != "" {
		cpuf, err := os.Create(*cpuprofile)
		if err != nil {
			log.Fatal(err)
		}
		pprof.StartCPUProfile(cpuf)
		defer pprof.StopCPUProfile()
	}

	if *memprofile != "" {
		defer func(file string) {
			memf, err := os.Create(file)
			if err != nil {
				log.Fatal(err)
			}
			pprof.WriteHeapProfile(memf)
		}(*memprofile)
	}

	// Working directory, where all transient stuff happens
	u, err := user.Current()
	if err != nil {
		log.Fatal("Couldn't watch dir: ", err)
	}
	pathArgs := []string{u.HomeDir, ".local", "share", "rakoshare"}
	workDir := filepath.Join(pathArgs...)

	app := cli.NewApp()
	app.Name = "rakoshare"
	app.Usage = "Share content with everyone"
	app.Commands = []cli.Command{
		{
			Name:  "gen",
			Usage: "Generate a share with a given target directory. Outputs the 3-tuple of id",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "dir",
					Value: "",
					Usage: "The directory to share",
				},
			},
			Action: func(c *cli.Context) {
				if c.String("dir") == "" {
					fmt.Println("Need a valid directory!")
					fmt.Println("Use the -dir flag")
					return
				}
				err := Generate(c.String("dir"), workDir)
				if err != nil {
					fmt.Println(err)
				}
			},
		},
		{
			Name:  "share",
			Usage: "Share the given id",
			Flags: []cli.Flag{
				cli.StringFlag{
					Name:  "id",
					Value: "",
					Usage: "The id to share",
				},
				cli.StringFlag{
					Name:  "dir",
					Value: "",
					Usage: "If not empty, the dir to share",
				},
			},
			Action: func(c *cli.Context) {
				if c.String("id") == "" {
					fmt.Println("Need an id!")
					return
				}
				Share(c.String("id"), workDir, c.String("dir"))
			},
		},
	}

	app.Run(os.Args)
}

func Share(cliId string, workDir string, cliTarget string) {
	shareID, err := id.NewFromString(cliId)
	if err != nil {
		fmt.Printf("Couldn't generate shareId: %s\n", err)
		return
	}
	sessionName := hex.EncodeToString(shareID.Infohash) + ".sql"
	session, err := sharesession.New(filepath.Join(workDir, sessionName))
	if err != nil {
		log.Fatal("Couldn't open session file: ", err)
	}

	fmt.Printf("WriteReadStore:\t%s\n     ReadStore:\t%s\n         Store:\t%s\n",
		shareID.WRS(), shareID.RS(), shareID.S())

	// Watcher
	target := session.GetTarget()
	if target == "" {
		if cliTarget == "" {
			fmt.Println("Need a folder to share!")
			return
		}
		target = cliTarget
		session.SaveSession(target, shareID)
	} else if cliTarget != "" {
		fmt.Printf("Can't override folder already set to %s\n", target)
	}
	_, err = os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			os.MkdirAll(target, 0744)
		} else {
			fmt.Printf("%s is an invalid dir: %s\n", target, err)
			os.Exit(1)
		}
	}
	watcher, err := NewWatcher(session, filepath.Clean(target), shareID.CanWrite())
	if err != nil {
		log.Fatal("Couldn't start watcher: ", err)
	}

	log.Println("Starting.")

	// External listener
	conChan, listenPort, err := listenForPeerConnections([]byte(shareID.Psk[:]))
	if err != nil {
		log.Fatal("Couldn't listen for peers connection: ", err)
	}

	var currentSession TorrentSessionI = EmptyTorrent{}

	// quitChan
	quitChan := listenSigInt()

	// LPD
	lpd := &Announcer{announces: make(chan *Announce)}
	if *useLPD {
		lpd, err = NewAnnouncer(listenPort)
		if err != nil {
			log.Fatal("Couldn't listen for Local Peer Discoveries: ", err)
		}
	}

	// Control session
	controlSession, err := NewControlSession(shareID, listenPort, session)
	if err != nil {
		log.Fatal(err)
	}
	if *useLPD {
		lpd.Announce(string(shareID.Infohash))
	}

mainLoop:
	for {
		select {
		case <-quitChan:
			err := currentSession.Quit()
			if err != nil {
				log.Println("Failed: ", err)
			} else {
				log.Println("Done")
			}
			break mainLoop
		case c := <-conChan:
			if currentSession.Matches(c.infohash) {
				currentSession.AcceptNewPeer(c)
			} else if controlSession.Matches(c.infohash) {
				controlSession.AcceptNewPeer(c)
			}
		case announce := <-lpd.announces:
			hexhash, err := hex.DecodeString(announce.infohash)
			if err != nil {
				log.Println("Err with hex-decoding:", err)
				break
			}
			if controlSession.Matches(string(hexhash)) {
				controlSession.hintNewPeer(announce.peer)
			}
		case ih := <-watcher.PingNewTorrent:
			if ih == controlSession.currentIH && !currentSession.IsEmpty() {
				break
			}

			controlSession.SetCurrent(ih)

			currentSession.Quit()

			torrentFile := session.GetCurrentTorrent()
			tentativeSession, err := NewTorrentSession(shareID, target, torrentFile, listenPort)
			if err != nil {
				if !os.IsNotExist(err) {
					log.Println("Couldn't start new session from watched dir: ", err)
				}

				// Fallback to an emptytorrent, because the previous one is
				// invalid; hope it will be ok next time !
				currentSession = EmptyTorrent{}
				break
			}
			currentSession = tentativeSession
			go currentSession.DoTorrent()
			for _, peer := range controlSession.peers {
				currentSession.hintNewPeer(peer.address)
			}
		case announce := <-controlSession.Torrents:
			controlSession.SetCurrent(announce.infohash)

			currentSession.Quit()

			magnet := fmt.Sprintf("magnet:?xt=urn:btih:%x", announce.infohash)
			tentativeSession, err := NewTorrentSession(shareID, target, magnet, listenPort)
			if err != nil {
				log.Println("Couldn't start new session from announce: ", err)
				currentSession = EmptyTorrent{}
				break
			}
			currentSession = tentativeSession
			go currentSession.DoTorrent()
			currentSession.hintNewPeer(announce.peer)
		case peer := <-controlSession.NewPeers:
			if currentSession.IsEmpty() {
				magnet := fmt.Sprintf("magnet:?xt=urn:btih:%x", controlSession.currentIH)
				tentativeSession, err := NewTorrentSession(shareID, target, magnet, listenPort)
				if err != nil {
					log.Printf("Couldn't start new session with new peer: %s\n", err)
					break
				}
				currentSession = tentativeSession
				go currentSession.DoTorrent()
			}
			currentSession.hintNewPeer(peer)
		case meta := <-currentSession.NewMetaInfo():
			watcher.saveMetainfo(meta)
		}
	}
}

type EmptyTorrent struct{}

func (et EmptyTorrent) Quit() error                 { return nil }
func (et EmptyTorrent) Matches(ih string) bool      { return false }
func (et EmptyTorrent) AcceptNewPeer(btc *btConn)   {}
func (et EmptyTorrent) DoTorrent()                  {}
func (et EmptyTorrent) hintNewPeer(peer string)     {}
func (et EmptyTorrent) IsEmpty() bool               { return true }
func (et EmptyTorrent) NewMetaInfo() chan *MetaInfo { return nil }

func listenSigInt() chan os.Signal {
	c := make(chan os.Signal)
	signal.Notify(c, os.Interrupt, os.Kill)
	return c
}
