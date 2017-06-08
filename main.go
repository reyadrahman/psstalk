package main

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"context"
	"flag"
	"github.com/nolash/psstalk/term"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/p2p"
	"github.com/ethereum/go-ethereum/pot"
	"github.com/ethereum/go-ethereum/p2p/protocols"
	"github.com/ethereum/go-ethereum/swarm/network"
	"github.com/ethereum/go-ethereum/node"
	pss "github.com/ethereum/go-ethereum/swarm/pss/client"
	"os"

	termbox "github.com/nsf/termbox-go"
)

// command line arguments
var (
	pssclienthost string
	pssclientport int
)

var (
	debugcount int
	myNick string = "self"
	run bool = true
	freeze bool = false
	chatlog log.Logger
)

func init() {
	hs := log.StreamHandler(os.Stderr, log.TerminalFormat(true))
	hf := log.LvlFilterHandler(log.LvlTrace, hs)
	h := log.CallerFileHandler(hf)
	log.Root().SetHandler(h)
	chatlog = log.New("chatlog", "main")

	flag.StringVar(&pssclienthost, "h", "localhost", "pss websocket hostname")
	flag.IntVar(&pssclientport, "p", node.DefaultWSPort, "pss websocket port")
}

func main() {
	var err error
	var ctx context.Context
	var cancel func()
	var psc *pss.Client

	quitC := make(chan struct{})

	// screen update trigger channels
	meC := make(chan []rune) // I send a message
	otherC := make(chan []rune) // Others send me a message
	promptC := make(chan bool) // Keyboard input

	// message channels
	inC := make(chan *chatMsg) // incoming message
	outC := make(chan interface{}) // outgoing message

	// initialize the terminal overlay handler
	client = term.NewTalkClient(2)

	// prompt buffers user input
	prompt.Reset()

	// use context for simple teardown
	ctx, cancel = context.WithCancel(context.Background())

	// connect to the pss backend
	// pssclient is a protocol mounted websocket RPC wrapper
	chatlog.Info("Connecting to pss websocket on %s:%d", pssclienthost, pssclientport)
	psc, err = connect(ctx, cancel, inC, outC, pssclienthost, pssclientport)
	if err != nil {
		chatlog.Crit(err.Error())
		os.Exit(1)
	}

	// start the termbox display
	err = startup()
	if err != nil {
		chatlog.Crit(err.Error())
		os.Exit(1)
	}

	// handle incoming messages
	go func() {
		for run {
			select {
				case chatmsg := <-inC:
					var rs []rune
					var buf *bytes.Buffer
//					buf = bytes.NewBufferString(fmt.Sprintf("%d=", chatmsg.Serial))
//					for {
//						r, n, err := buf.ReadRune()
//						if err != nil || n == 0 {
//							break
//						}
//						rs = append(rs, r)
//					}

					buf = bytes.NewBuffer(chatmsg.Content)
					for {
						r, n, err := buf.ReadRune()
						if err != nil || n == 0 {
							break
						}
						rs = append(rs, r)
					}
					client.Buffers[1].Add(getSrc(chatmsg.Source), rs)
					otherC <- rs
			}
		}
	}()

	// update terminal screen loop
	go func() {
		for run {
			select {
			case <-meC:
				updateView(client.Buffers[0], 0, client.Lines[0]-1)
				termbox.Flush()
			case <-promptC:
				termbox.SetCursor(prompt.Count%client.Width, prompt.Line+(prompt.Count/client.Width))
				termbox.Flush()
			case <-otherC:
				updateView(client.Buffers[1], client.Lines[0]+1, client.Lines[1])
				termbox.Flush()
			case <-quitC:
				run = false
			}
		}
	}()

	// handle input

	termbox.SetCursor(0, 0)

	for run {
		before := prompt.Count / client.Width
		ev := termbox.PollEvent()
		if ev.Type == termbox.EventKey {
			if ev.Ch == 0 {
				switch ev.Key {
				// esc quits the application
				case termbox.KeyEsc:
					quitC <- struct{}{}
					run = false
				// pop from prompt buffer
				// if the line count changes also update the message buffer, less the lines that the prompt buffer occupies
				case termbox.KeyBackspace:
					removeFromPrompt(before)
					promptC <- true
				case termbox.KeyBackspace2:
					removeFromPrompt(before)
					promptC <- true
				// enter sends the message
				case termbox.KeyEnter:
					line := prompt.Buffer
					res, payload, err := client.Process(line)
					if err == nil {
						if client.IsAddCmd() {
							args := client.GetCmd()
							b, _ := hex.DecodeString(args[1])
							potaddr := pot.Address{}
							copy(potaddr[:], b[:])
							psc.AddPssPeer(potaddr, chatProtocol)
						} else if client.IsSendCmd() && len(client.Sources) == 0 {
							res = "noone to send to ... add someone first"
							err = fmt.Errorf("no receivers")
						} else {
							if payload != "" {
								// dispatch message
								payload := chatMsg{
									Serial:  uint64(client.Buffers[0].Count()),
									Content: []byte(payload),
									Source:  randomSrc().Nick,
								}

								outC <- payload
							}

							// add the line to the history buffer for the local user
							client.Buffers[0].Add(nil, line)
						}

						// move the prompt line down
						// and back up if we hit the bottom of the viewport height
						prompt.Line += (prompt.Count / client.Width) + 1
						if prompt.Line > client.Lines[0]-1 {
							prompt.Line = client.Lines[0] - 1
						}

						// update the local user viewport
						meC <- line

						// clear the prompt buffer
						prompt.Reset()

						// clear the prompt line in the viewport
						for i := 0; i < client.Width; i++ {
							termbox.SetCell(i, prompt.Line, runeSpace, bgAttr, bgAttr)
						}

						// update the prompt in the viewport
						// (do we need this?)
						promptC <- true

					}

					if len(res) > 0 {
						resrunes := bytes.Runes([]byte(res))
						client.Buffers[1].Add(nil, resrunes)
						otherC <- resrunes
					}

				case termbox.KeySpace:
					addToPrompt(runeSpace, before)
					promptC <- true
				}
			} else {
				addToPrompt(ev.Ch, before)
				promptC <- true

			}
		}
	}

	_ = psc

	shutdown()
}

func connect(ctx context.Context, cancel func(), inC chan *chatMsg, outC chan interface{}, host string, port int) (*pss.Client, error) {
	var err error

	cfg := pss.NewClientConfig()
	cfg.RemoteHost = host
	cfg.RemotePort = port
	pssbackend := pss.NewClient(ctx, cancel, cfg)
	err = pssbackend.Start()
	if err != nil {
		return nil, newError(ePss, err.Error())
	}
	err = pssbackend.RunProtocol(newProtocol(inC, outC))
	if err != nil {
		return nil, newError(ePss, err.Error())
	}
	return pssbackend, nil
}

func newProtocol(inC chan *chatMsg, outC chan interface{}) *p2p.Protocol {
	chatctrl := chatCtrl{
		inC: inC,
	}
	return &p2p.Protocol{
		Name:    chatProtocol.Name,
		Version: chatProtocol.Version,
		Length:  3,
		Run: func(p *p2p.Peer, rw p2p.MsgReadWriter) error {
			peerid := p.ID()
			chatctrl.oAddr = network.ToOverlayAddr(peerid[:])
			pp := protocols.NewPeer(p, rw, chatProtocol)
			if outC != nil {
				go func() {
					for {
						select {
						case msg := <-outC:
							err := pp.Send(msg)
							if err != nil {
								chatlog.Error("Could not send to peer", "id", p.ID(), "peer", chatctrl.oAddr, "err", err)
								pp.Drop(err)
								return
							}
						}
					}
				}()
			}
			pp.Run(chatctrl.chatHandler)
			return nil
		},
	}
}
