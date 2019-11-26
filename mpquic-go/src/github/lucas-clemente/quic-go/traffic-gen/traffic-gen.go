package main

import (
	"bytes"
	crand "crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/pem"

	"sync"

	// "errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"math/rand"
	"net"

	// "net/http"
	// "encoding/base64"
	"os"
	"strconv"
	"strings"
	"time"

	quic "github.com/lucas-clemente/quic-go"

	// "github.com/lucas-clemente/quic-go/h2quic"
	// "github.com/lucas-clemente/quic-go/internal/testdata"
	"github.com/lucas-clemente/quic-go/internal/utils"
	// "quic-go"
	//	"io/ioutil"
	"container/list"
)

type binds []string

type ServerLog struct {
	timeStamps map[uint]uint
	lock       sync.RWMutex
}

func (b binds) String() string {
	return strings.Join(b, ",")
}

func (b *binds) Set(v string) error {
	*b = strings.Split(v, ",")
	return nil
}

type MessageList struct {
	mess_list *list.List
	// isSending bool
	mutex sync.RWMutex
}

var BASE_SEQ_NO uint = 2147483648 // 0x80000000
var LOG_PREFIX string = ""
var SERVER_ADDRESS string = "10.1.1.2"
var SERVER_TCP_PORT int = 2121
var SERVER_QUIC_PORT int = 4343
var STREAM_TIMEOUT int = 5

type ClientManager struct {
	clients    map[*Client]bool
	broadcast  chan []byte
	register   chan *Client
	unregister chan *Client
}

type Client struct {
	socket net.Conn
	data   chan []byte
}

func (manager *ClientManager) start() {
	for {
		select {
		case connection := <-manager.register:
			manager.clients[connection] = true
			fmt.Println("Added new connection!")
		case connection := <-manager.unregister:
			if _, ok := manager.clients[connection]; ok {
				close(connection.data)
				delete(manager.clients, connection)
				fmt.Println("A connection has terminated!")
			}
		case message := <-manager.broadcast:
			for connection := range manager.clients {
				select {
				case connection.data <- message:
				default:
					close(connection.data)
					delete(manager.clients, connection)
				}
			}
		}
	}
}

func (manager *ClientManager) receive(client *Client) {

	timeStamps := make(map[uint]uint)
	buffer := make([]byte, 0)
	for {
		message := make([]byte, 65536)
		length, err := client.socket.Read(message)
		if err != nil {
			log.Println(err)
			manager.unregister <- client
			client.socket.Close()
			break
		}
		if length > 0 {
			message = message[0:length]
			// utils.Debugf("\n RECEIVED: %x \n", message)
			// manager.broadcast <- message
			eoc_byte_index := bytes.Index(message, intToBytes(uint(BASE_SEQ_NO-1), 4))
			// log.Println(eoc_byte_index)

			for eoc_byte_index != -1 {
				data_chunk := append(buffer, message[0:eoc_byte_index+4]...)
				//				seq_no := message[eoc_byte_index-4:eoc_byte_index]
				// utils.Debugf("\n CHUNK: %x \n  length %d \n", data_chunk, len(data_chunk))
				// Get data chunk ID and record receive timestampt
				seq_no := data_chunk[0:4]
				seq_no_int := bytesToInt(seq_no)
				timeStamps[seq_no_int] = uint(time.Now().UnixNano())
				//				buffer.Write(message[eoc_byte_index:length])

				// Cut out recorded chunk
				message = message[eoc_byte_index+4:]
				buffer = make([]byte, 0)
				eoc_byte_index = bytes.Index(message, intToBytes(uint(BASE_SEQ_NO-1), 4))
			}
			buffer = append(buffer, message...)
		}
	}

	writeToFile(LOG_PREFIX+"server-timestamp.log", timeStamps)
}

// func (client *Client) receive() {
// 	for {
// 		message := make([]byte, 4096)
// 		length, err := client.socket.Read(message)
// 		if err != nil {
// 			client.socket.Close()
// 			break
// 		}
// 		if length > 0 {
// 			utils.Debugf("RECEIVED: " + string(message))
// 		}
// 	}
// }

func (manager *ClientManager) send(client *Client) {
	defer client.socket.Close()
	for {
		select {
		case message, ok := <-client.data:
			if !ok {
				return
			}
			client.socket.Write(message)
		}
	}
}

func startServerMode(address string, protocol string, multipath bool, log_file string) {
	fmt.Println("Starting server...")
	var listener net.Listener
	var err error
	manager := ClientManager{
		clients:    make(map[*Client]bool),
		broadcast:  make(chan []byte),
		register:   make(chan *Client),
		unregister: make(chan *Client),
	}
	go manager.start()

	switch protocol {
	case "tcp":

		listener, err = net.Listen("tcp", address)
		if err != nil {
			log.Println(err)
		}
		log.Println("TCP Listen ...")
		for {
			connection, _ := listener.Accept()
			tcp_connection := connection.(*net.TCPConn)
			tcp_connection.SetNoDelay(true)
			if err != nil {
				log.Println(err)
			}
			client := &Client{socket: tcp_connection, data: make(chan []byte)}
			manager.register <- client
			go manager.receive(client)
			//		go manager.send(client)
		}
	case "quic":

		startQUICServer(address, multipath)

	}

}

func startClientMode(address string, protocol string, run_time uint, csize_distro string, csize_value float64, arrival_distro string, arrival_value float64, multipath bool, scheduler string, isBlockingCall bool, isMultiStream bool) {
	fmt.Println("Starting client...")

	// var stream quic.Stream
	var quic_session quic.Session
	var connection *net.TCPConn
	var err error

	if protocol == "quic" {
		addresses := []string{address}
		quic_session, err = startQUICSession(addresses, scheduler, multipath)
		// defer stream.Close()
		defer quic_session.Close(nil)

	} else if protocol == "tcp" {
		tcp_address := strings.Split(address, ":")
		ip_add := net.ParseIP(tcp_address[0]).To4()
		port, _ := strconv.Atoi(tcp_address[1])
		connection, err = net.DialTCP("tcp", nil, &net.TCPAddr{IP: ip_add, Port: port})
		connection.SetNoDelay(true)
		defer connection.Close()

	}
	//	addr,_:=net.ResolveTCPAddr("tcp", address+":443")
	//	connection, error := net.DialTCP("tcp", nil, addr)

	if err != nil {
		log.Println(err)
	}

	//	error = connection.SetNoDelay(true)
	//	if error != nil {
	//	    log.Println(error.Error())
	//	}
	//	client := &Client{socket: connection}
	//	go client.receive()

	sendingDone := make(chan bool)
	generatingDone := make(chan bool)
	//	go client.send(connection ,run_time , csize_distro , csize_value , arrival_distro , arrival_value )

	// go func() {
	var run_time_duration time.Duration
	run_time_duration, err = time.ParseDuration(strconv.Itoa(int(run_time)) + "ms")
	if err != nil {
		log.Println(err)
	}

	startDelay, _ := time.ParseDuration("5s")
	startTime := time.Now().Add(startDelay)
	endTime := startTime.Add(run_time_duration)
	timeStamps := make(map[uint]uint)

	// writeTime := make(map[uint]uint)
	send_queue := MessageList{mess_list: list.New()}
	gen_finished := false

	go func() {
		gen_counter := 1
		for i := 1; time.Now().Before(endTime); i++ {
			if isBlockingCall && send_queue.mess_list.Len() > 0 {
				time.Sleep(time.Nanosecond)
				continue
			}
			// reader := bufio.NewReader(os.Stdin)
			// message, _ := reader.ReadString('\n')
			//			utils.Debugf("before: %d \n", time.Now().UnixNano())
			message, seq := generateMessage(uint(gen_counter), csize_distro, csize_value)
			gen_counter++
			// send_queue = append(send_queue, message)
			// next_message := send_queue[0]

			// if !isBlockingCall {
			var wait_time uint
			if time.Now().Before(startTime) {
				wait_time = 1000000000
			} else {
				wait_time = uint(1000000000/getRandom(arrival_distro, arrival_value)) - (uint(time.Now().UnixNano()) - timeStamps[seq-1])
			}

			if wait_time > 0 {
				wait(wait_time)
			}
			// }

			// if !isBlockingCall {
			// Get time at the moment message generated
			timeStamps[seq] = uint(time.Now().UnixNano())
			// utils.Debugf("Messages in queue: %d \n", len(send_queue))

			// }
			send_queue.mutex.Lock()
			send_queue.mess_list.PushBack(message)
			send_queue.mutex.Unlock()

			// writeTime[seq] = uint(time.Now().UnixNano()) - timeStamps[seq]

			// remove sent file from the queue
			// send_queue = send_queue[1:]

			// utils.Debugf("PUT: %d \n", seq)

		}
		utils.Debugf("Generate total: %d messages", gen_counter)
		gen_finished = true
		generatingDone <- true
	}()

	go func() {
		sent_counter := 0
		var current_stream quic.Stream

		for !gen_finished {
			time.Sleep(time.Nanosecond)
			if send_queue.mess_list.Len() == 0 {
				continue
			}
			queue_font := send_queue.mess_list.Front()
			message, _ := queue_font.Value.([]byte)
			// beforesent := time.Now()
			// if isBlockingCall {
			// 	// Get time at the moment message put in stream
			// 	timeStamps[bytesToInt(message[0:4])] = uint(time.Now().UnixNano())
			// }

			// send_queue.mutex.Lock()
			// send_queue.isSending = true
			// send_queue.mutex.Unlock()

			// utils.Debugf("Time in queue: %d \n", uint(time.Now().UnixNano())-timeStamps[bytesToInt(message[0:4])])
			if protocol == "quic" {
				if isMultiStream {

					go startQUICClientStream(quic_session, message)
				} else {
					if current_stream == nil || current_stream.StreamID() == 3 {
						current_stream, err = quic_session.OpenStreamSync()
						if err != nil {
							utils.Debugf("Error OpenStreamSync:", err)
							return
						}

						defer current_stream.Close()
					}
					utils.Debugf("OpenStream count: %d", quic_session.GetOpenStreamNo())
					beforeWrite := time.Now()
					current_stream.Write(message)
					utils.Debugf("StreamID: %d write %d", current_stream.StreamID(), time.Now().Sub(beforeWrite).Nanoseconds())

				}

			} else if protocol == "tcp" {
				go connection.Write(message)

			}

			sent_counter++
			send_queue.mutex.Lock()
			// send_queue.isSending = false
			send_queue.mess_list.Remove(queue_font)
			send_queue.mutex.Unlock()

			// if isBlockingCall {
			// 	// utils.Debugf("Sent time: %d ", time.Now().Sub(beforesent).Nanoseconds())
			// 	var wait_time uint
			// 	if time.Now().Before(startTime) {
			// 		wait_time = 1000000000
			// 	} else {
			// 		wait_time = uint(1000000000/getRandom(arrival_distro, arrival_value)) - (uint(time.Now().UnixNano()) - timeStamps[bytesToInt(message[0:4])])
			// 		// wait_time = uint(1000000000/getRandom(arrival_distro, arrival_value)) - (uint(time.Now().UnixNano()) - timeStamps[seq-1])

			// 	}
			// 	if wait_time > 0 {
			// 		wait(wait_time)
			// 	}
			// }

		}
		utils.Debugf("Sent total: %d messages", sent_counter)

		sendingDone <- true

	}()
	<-generatingDone
	<-sendingDone
	writeToFile(LOG_PREFIX+"client-timestamp.log", timeStamps)
	// writeToFile(LOG_PREFIX+"write-timegap.log", writeTime)
	os.Rename("sender-frame.log", LOG_PREFIX+"sender-frame.log")
	os.Rename("receiver-frame.log", LOG_PREFIX+"receiver-frame.log")

	// }()
}

func startQUICClientStream(quic_session quic.Session, message []byte) {
	beforeOpen := time.Now()
	stream, err := quic_session.OpenStreamSync()
	if err != nil {
		utils.Debugf("Error OpenStreamSync:", err)
		return
	}
	defer stream.Close()
	utils.Debugf("OpenStream count: %d", quic_session.GetOpenStreamNo())
	beforeWrite := time.Now()
	stream.Write(message)
	utils.Debugf("StreamID: %d open %d write %d", stream.StreamID(), beforeWrite.Sub(beforeOpen).Nanoseconds(), time.Now().Sub(beforeWrite).Nanoseconds())
	quic_session.RemoveStream(stream.StreamID())
}

func startQUICServer(addr string, isMultipath bool) error {

	listener, err := quic.ListenAddr(addr, generateTLSConfig(), &quic.Config{
		CreatePaths: isMultipath,
	})
	if err != nil {
		return err
	}
	sess, err := listener.Accept()
	if err != nil {
		return err
	}
	defer sess.Close(err)

	serverlog := ServerLog{timeStamps: make(map[uint]uint), lock: sync.RWMutex{}}

	// previous := BASE_SEQ_NO

	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			utils.Errorf("AcceptStream: ", err)
			break
		}
		// defer stream.Close()
		go startServerStream(stream, &serverlog)

	}

	writeToFile(LOG_PREFIX+"server-timestamp.log", serverlog.timeStamps)
	fmt.Printf("\nFinish receive: %d messages \n", len(serverlog.timeStamps))
	return err
}

func startServerStream(stream quic.Stream, serverlog *ServerLog) {
	utils.Debugf("\n Get data from stream: %d \n", stream.StreamID())
	beginstream := time.Now()
	buffer := make([]byte, 0)
	defer stream.Close()
	// prevTime := time.Now()
	// deadline := time.Now().Add(time.Duration(STREAM_TIMEOUT) * time.Second)
messageLoop:
	for {
		readTime := time.Now()
		// if time.Now().After(deadline) {
		// 	stream.Close()
		// 	break
		// }
		message := make([]byte, 65536)
		length, err := stream.Read(message)

		if length > 0 {
			// deadline = time.Now().Add(time.Duration(STREAM_TIMEOUT) * time.Second)
			message = message[0:length]
			// utils.Debugf("\n after %d RECEIVED from stream %d mes_len %d buffer %d: %x...%x \n", time.Now().Sub(prevTime).Nanoseconds(), stream.StreamID(), length, len(buffer), message[0:4], message[length-4:length])
			// prevTime = time.Now()
			eoc_byte_index := bytes.Index(message, intToBytes(uint(BASE_SEQ_NO-1), 4))
			// log.Println(eoc_byte_index)

			for eoc_byte_index != -1 {
				data_chunk := append(buffer, message[0:eoc_byte_index+4]...)
				//				seq_no := message[eoc_byte_index-4:eoc_byte_index]
				// utils.Debugf("\n CHUNK: %x...%x  \n  length %d \n", data_chunk[0:4], data_chunk[len(data_chunk)-4:len(data_chunk)], len(data_chunk))
				// Get data chunk ID and record receive timestampt
				seq_no := data_chunk[0:4]
				seq_no_int := bytesToInt(seq_no)

				// these lines to debug
				// if seq_no_int != previous+1 {
				// 	utils.Debugf("\n Unordered: %d \n", seq_no_int)
				// }
				// previous = seq_no_int
				//
				if seq_no_int >= BASE_SEQ_NO {
					utils.Debugf("\n Got seq: %d \n", seq_no_int)
					serverlog.lock.Lock()
					serverlog.timeStamps[seq_no_int] = uint(readTime.UnixNano())
					serverlog.lock.Unlock()

				}
				//				buffer.Write(message[eoc_byte_index:length])

				// Cut out recorded chunk
				message = message[eoc_byte_index+4:]
				buffer = make([]byte, 0)
				eoc_byte_index = bytes.Index(message, intToBytes(uint(BASE_SEQ_NO-1), 4))
			}
			buffer = append(buffer, message...)
		}

		if err != nil {
			// log.Println(err)

			break messageLoop
		}
	}

	utils.Debugf("\n Finish Stream: %d in %d ns \n", stream.StreamID(), time.Now().Sub(beginstream).Nanoseconds())
}

func startQUICSession(urls []string, scheduler string, isMultipath bool) (sess quic.Session, err error) {

	session, err := quic.DialAddr(urls[0], &tls.Config{InsecureSkipVerify: true}, &quic.Config{
		CreatePaths: isMultipath,
	})

	if err != nil {
		return nil, err
	}
	quic.SetSchedulerAlgorithm(scheduler)

	return session, nil
}

//func (client *Client) send(connection net.Conn,run_time uint, csize_distro string, csize_value float64, arrival_distro string, arrival_value float64) {
//
//	run_time_duration, error := time.ParseDuration(strconv.Itoa(int(run_time)) + "ms")
//	if error != nil {
//		log.Println(error)
//	}
//
//	startTime := time.Now()
//	timeStamps := make(map[uint]uint)
//	for i:=1; time.Now().Sub(startTime) < run_time_duration;i++ {
//		// reader := bufio.NewReader(os.Stdin)
//		// message, _ := reader.ReadString('\n')
//		message, seq_no := generateMessage(uint(i),csize_distro, csize_value)
//		connection.Write(message)
//		timeStamps[seq_no] = uint(time.Now().UnixNano())
//		wait(getRandom(arrival_distro, arrival_value))
//	}
//	writeToFile("client-timestamp.log", timeStamps)
//
//}

// wait for interarrival_time nanosecond
func wait(interarrival_time uint) {
	waiting_time := time.Duration(interarrival_time) * time.Nanosecond
	// utils.Debugf("wait for %d ns \n", waiting_time.Nanoseconds())
	time.Sleep(waiting_time)
}

func getRandom(distro string, value float64) float64 {
	var retVal float64
	switch distro {
	case "c":
		retVal = value
	case "e":
		retVal = rand.ExpFloat64() * value
	case "g":
		retVal = rand.NormFloat64()*value/3 + value
		if retVal > 2*value {
			retVal = 2 * value
		} else if retVal < 0 {
			retVal = 0
		}

	case "b":

	case "wei":

	default:
		retVal = 1.0
	}

	return retVal
}

func generateMessage(offset_seq uint, csize_distro string, csize_value float64) ([]byte, uint) {
	//	utils.Debugf("Gen mess: %d \n", time.Now().UnixNano())
	seq_no := BASE_SEQ_NO + offset_seq
	seq_header := intToBytes(uint(seq_no), 4)
	eoc_header := intToBytes(uint(BASE_SEQ_NO-1), 4)

	csize := uint64(getRandom(csize_distro, csize_value))
	//chunk size must be a factor of 4 to avoid EOL fragmenting
	// Temporary set to a factor of 4 to match QUIC MTU 1350byte
	csize = csize - csize%2
	if csize < 12 {
		csize = 12
	}
	// utils.Debugf("Message size %d \n ", csize)
	pseudo_payload := make([]byte, (csize - 8))
	for i := 0; i < len(pseudo_payload); i++ {
		pseudo_payload[i] = 0x01
	}

	message := append(seq_header, pseudo_payload...)
	//	message = append(message, seq_header...)
	message = append(message, eoc_header...)

	return message, seq_no
}

func intToBytes(num uint, size uint) []byte {
	bs := make([]byte, size)
	binary.BigEndian.PutUint32(bs, uint32(num))
	return bs
}

func bytesToInt(b []byte) uint {
	return uint(binary.BigEndian.Uint32(b))
}

func writeToFile(filename string, data map[uint]uint) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	for k, v := range data {

		_, err = io.WriteString(file, fmt.Sprintln(k, v))
		if err != nil {
			return err
		}
	}

	return file.Sync()
}

type loggingWriter struct{ io.Writer }

func (w loggingWriter) Write(b []byte) (int, error) {
	fmt.Printf("Server: Got '%x'\n", b)
	return w.Writer.Write(b)
}

func generateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(crand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(crand.Reader, &template, &template, &key.PublicKey, key)

	if err != nil {
		panic(err)
	}

	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	// pkcs1 := x509.MarshalPKCS1PrivateKey(key)
	// priv := base64.StdEncoding.EncodeToString(pkcs1)
	// pub := base64.StdEncoding.EncodeToString(x509.MarshalPKCS1PublicKey(&key.PublicKey))

	// utils.Debugf("pri key: %s", priv)
	// utils.Debugf("pub key: %s", pub)
	// key_file, _ := os.Create("id_rsa_trafficgen")

	// defer key_file.Close()
	// key_file.WriteString("-----BEGIN RSA PRIVATE KEY-----\n")
	// key_file.WriteString(priv)
	// key_file.WriteString("\n-----END RSA PRIVATE KEY-----")

	if err != nil {
		panic(err)
	}
	return &tls.Config{Certificates: []tls.Certificate{tlsCert}}
}

func schedNameConvert(protocol string, sched_name string) string {
	converted_name := sched_name
	if protocol == "quic" {
		switch sched_name {
		case "lrtt":
			converted_name = "lowRTT"
		case "rr":
			converted_name = "RR"
		case "opp":
			converted_name = "oppRedundant"

		case "nt":
			converted_name = "nineTails"
		default:
			panic("no scheduler found")
		}
	}

	return converted_name
}

func main() {
	flagMode := flag.String("mode", "server", "start in client or server mode")
	flagTime := flag.Uint("t", 10000, "time to run (ms)")
	flagCsizeDistro := flag.String("csizedist", "c", "data chunk size distribution")
	flagCsizeValue := flag.Float64("csizeval", 1000, "data chunk size value")
	flagArrDistro := flag.String("arrdist", "c", "arrival distribution")
	flagArrValue := flag.Float64("arrval", 1000, "arrival value")
	flagAddress := flag.String("a", "localhost", "Destination address")
	flagProtocol := flag.String("p", "tcp", "TCP or QUIC")
	flagLog := flag.String("log", "", "Log folder")
	flagMultipath := flag.Bool("m", false, "Enable multipath")
	flagMultiStream := flag.Bool("mstr", false, "Enable multistream")
	flagSched := flag.String("sched", "lrtt", "Scheduler")
	flagDebug := flag.Bool("v", false, "Debug mode")
	flagCong := flag.String("cc", "olia", "Congestion control")
	flagBlock := flag.Bool("b", false, "Blocking call")
	flag.Parse()
	if *flagDebug {
		utils.SetLogLevel(utils.LogLevelDebug)
	}

	LOG_PREFIX = *flagLog
	quic.SetCongestionControl(*flagCong)
	sched := schedNameConvert(*flagProtocol, *flagSched)
	if strings.ToLower(*flagMode) == "server" {
		quic.SetSchedulerAlgorithm(sched)
		startServerMode(*flagAddress, *flagProtocol, *flagMultipath, *flagLog)
	} else {
		startClientMode(*flagAddress, *flagProtocol, *flagTime, *flagCsizeDistro, float64(*flagCsizeValue), *flagArrDistro, float64(*flagArrValue), *flagMultipath, sched, *flagBlock, *flagMultiStream)
	}
}
