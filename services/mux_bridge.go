package services

import (
	"bufio"
	"fmt"
	"proxy/utils"
	"io"
	"log"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xtaci/smux"
)

type MuxBridge struct {
	cfg                MuxBridgeArgs
	clientControlConns utils.ConcurrentMap
	serverConns        utils.ConcurrentMap
	router             utils.ClientKeyRouter
	l                  *sync.Mutex
	isStop             bool
	sc                 *utils.ServerChannel
}

func NewMuxBridge() Service {
	b := &MuxBridge{
		cfg:                MuxBridgeArgs{},
		clientControlConns: utils.NewConcurrentMap(),
		serverConns:        utils.NewConcurrentMap(),
		l:                  &sync.Mutex{},
		isStop:             false,
	}
	b.router = utils.NewClientKeyRouter(&b.clientControlConns, 50000)
	return b
}

func (s *MuxBridge) InitService() (err error) {
	return
}
func (s *MuxBridge) CheckArgs() (err error) {
	if *s.cfg.CertFile == "" || *s.cfg.KeyFile == "" {
		err = fmt.Errorf("cert and key file required")
		return
	}
	if *s.cfg.LocalType == TYPE_TLS {
		s.cfg.CertBytes, s.cfg.KeyBytes, err = utils.TlsBytes(*s.cfg.CertFile, *s.cfg.KeyFile)
		if err != nil {
			return
		}
	}
	return
}
func (s *MuxBridge) StopService() {
	defer func() {
		e := recover()
		if e != nil {
			log.Printf("stop bridge service crashed,%s", e)
		} else {
			log.Printf("service bridge stoped")
		}
	}()
	s.isStop = true
	if s.sc != nil && (*s.sc).Listener != nil {
		(*(*s.sc).Listener).Close()
	}
	for _, g := range s.clientControlConns.Items() {
		for _, session := range g.(utils.ConcurrentMap).Items() {
			(session.(*smux.Session)).Close()
		}
	}
	for _, c := range s.serverConns.Items() {
		(*c.(*net.Conn)).Close()
	}
}
func (s *MuxBridge) Start(args interface{}) (err error) {
	s.cfg = args.(MuxBridgeArgs)
	if err = s.CheckArgs(); err != nil {
		return
	}
	if err = s.InitService(); err != nil {
		return
	}

	host, port, _ := net.SplitHostPort(*s.cfg.Local)
	p, _ := strconv.Atoi(port)
	sc := utils.NewServerChannel(host, p)
	if *s.cfg.LocalType == TYPE_TCP {
		err = sc.ListenTCP(s.handler)
	} else if *s.cfg.LocalType == TYPE_TLS {
		err = sc.ListenTls(s.cfg.CertBytes, s.cfg.KeyBytes, nil, s.handler)
	} else if *s.cfg.LocalType == TYPE_KCP {
		err = sc.ListenKCP(s.cfg.KCP, s.handler)
	}
	if err != nil {
		return
	}
	s.sc = &sc
	log.Printf("%s bridge on %s", *s.cfg.LocalType, (*sc.Listener).Addr())
	return
}
func (s *MuxBridge) Clean() {
	s.StopService()
}
func (s *MuxBridge) handler(inConn net.Conn) {
	reader := bufio.NewReader(inConn)

	var err error
	var connType uint8
	var key string
	inConn.SetDeadline(time.Now().Add(time.Millisecond * time.Duration(*s.cfg.Timeout)))
	err = utils.ReadPacket(reader, &connType, &key)
	inConn.SetDeadline(time.Time{})
	if err != nil {
		log.Printf("read error,ERR:%s", err)
		return
	}
	switch connType {
	case CONN_SERVER:
		var serverID string
		inAddr := inConn.RemoteAddr().String()
		inConn.SetDeadline(time.Now().Add(time.Millisecond * time.Duration(*s.cfg.Timeout)))
		err = utils.ReadPacketData(reader, &serverID)
		inConn.SetDeadline(time.Time{})
		if err != nil {
			log.Printf("read error,ERR:%s", err)
			return
		}
		log.Printf("server connection %s %s connected", serverID, key)
		if c, ok := s.serverConns.Get(inAddr); ok {
			(*c.(*net.Conn)).Close()
		}
		s.serverConns.Set(inAddr, &inConn)
		session, err := smux.Server(inConn, nil)
		if err != nil {
			utils.CloseConn(&inConn)
			log.Printf("server session error,ERR:%s", err)
			return
		}
		for {
			if s.isStop {
				return
			}
			stream, err := session.AcceptStream()
			if err != nil {
				session.Close()
				utils.CloseConn(&inConn)
				s.serverConns.Remove(inAddr)
				log.Printf("server connection %s %s released", serverID, key)
				return
			}
			go func() {
				defer func() {
					if e := recover(); e != nil {
						log.Printf("bridge callback crashed,err: %s", e)
					}
				}()
				s.callback(stream, serverID, key)
			}()
		}
	case CONN_CLIENT:
		log.Printf("client connection %s connected", key)
		session, err := smux.Client(inConn, nil)
		if err != nil {
			utils.CloseConn(&inConn)
			log.Printf("client session error,ERR:%s", err)
			return
		}
		keyInfo := strings.Split(key, "-")
		if len(keyInfo) != 2 {
			utils.CloseConn(&inConn)
			log.Printf("client key format error,key:%s", key)
			return
		}
		groupKey := keyInfo[0]
		index := keyInfo[1]
		s.l.Lock()
		defer s.l.Unlock()
		if !s.clientControlConns.Has(groupKey) {
			item := utils.NewConcurrentMap()
			s.clientControlConns.Set(groupKey, &item)
		}
		_group, _ := s.clientControlConns.Get(groupKey)
		group := _group.(*utils.ConcurrentMap)
		if v, ok := group.Get(index); ok {
			v.(*smux.Session).Close()
		}
		group.Set(index, session)
		// s.clientControlConns.Set(key, session)
		go func() {
			for {
				if s.isStop {
					return
				}
				if session.IsClosed() {
					s.l.Lock()
					defer s.l.Unlock()
					if sess, ok := group.Get(index); ok && sess.(*smux.Session).IsClosed() {
						group.Remove(index)
						log.Printf("client connection %s released", key)
					}
					if group.IsEmpty() {
						s.clientControlConns.Remove(groupKey)
					}
					break
				}
				time.Sleep(time.Second * 5)
			}
		}()
		//log.Printf("set client session,key: %s", key)
	}

}
func (s *MuxBridge) callback(inConn net.Conn, serverID, key string) {
	try := 20
	for {
		if s.isStop {
			return
		}
		try--
		if try == 0 {
			break
		}
		if key == "*" {
			key = s.router.GetKey()
		}
		_group, ok := s.clientControlConns.Get(key)
		if !ok {
			log.Printf("client %s session not exists for server stream %s, retrying...", key, serverID)
			time.Sleep(time.Second * 3)
			continue
		}
		group := _group.(*utils.ConcurrentMap)
		keys := group.Keys()
		keysLen := len(keys)
		i := 0
		if keysLen > 0 {
			i = rand.Intn(keysLen)
		} else {
			log.Printf("client %s session empty for server stream %s, retrying...", key, serverID)
			time.Sleep(time.Second * 3)
			continue
		}
		index := keys[i]
		log.Printf("select client : %s-%s", key, index)
		session, _ := group.Get(index)
		session.(*smux.Session).SetDeadline(time.Now().Add(time.Millisecond * time.Duration(*s.cfg.Timeout)))
		stream, err := session.(*smux.Session).OpenStream()
		session.(*smux.Session).SetDeadline(time.Time{})
		if err != nil {
			log.Printf("%s client session open stream %s fail, err: %s, retrying...", key, serverID, err)
			time.Sleep(time.Second * 3)
			continue
		} else {
			log.Printf("stream %s -> %s created", serverID, key)
			die1 := make(chan bool, 1)
			die2 := make(chan bool, 1)
			go func() {
				io.Copy(stream, inConn)
				die1 <- true
			}()
			go func() {
				io.Copy(inConn, stream)
				die2 <- true
			}()
			select {
			case <-die1:
			case <-die2:
			}
			stream.Close()
			inConn.Close()
			log.Printf("%s server %s stream released", key, serverID)
			break
		}
	}

}
