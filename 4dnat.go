package main

import (
	"crypto/tls"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const RetryInterval int = 5
const Timeout int = 10

func init() {
	log.SetPrefix("[4dnat] ")
}

func main() {
	printBanner()
	args := os.Args

	if len(args) < 4 {
		printHelp()
		os.Exit(0)
	}

	switch args[1] {
	case "-listen", "-l":
		listener(args[2], args[3])
	case "-forward", "-f":
		forward(args[2], args[3])
	case "-agent", "-a":
		agent(args[2], args[3])
	case "-proxy", "-p":
		proxy(args[2], args[3], args)
	default:
		printHelp()
	}
}

func printHelp() {
	println("usage:")
	println(`    "-forward listenPort targetAddress" example: "-forward 10000 127.0.0.1:22"`)
	println(`    "-listen listenPort0 listenPort1" example: "-listen 10000 10001"`)
	println(`    "-agent targetAddress0 targetAddress1" example: "-agent 127.0.0.1:10000 127.0.0.1:22"`)
	println(`    "-proxy protocol listenAddress" example: "-proxy http 1080", "-proxy https 1080 server.pem server.key"`)
}

func printBanner() {
	println(`
   _____     .___             __   
  /  |  |  __| _/____ _____ _/  |_ 
 /   |  |_/ __ |/    \\__  \\   __\
/    ^   / /_/ |   |  \/ __ \|  |  
\____   |\____ |___|  (____  /__|  
     |__|     \/    \/     \/
`)
}

func copyIO(src, dest net.Conn, wg *sync.WaitGroup) {
	defer src.Close()
	log.Printf("[*] [%s->%s] --> [%s->%s]\n", src.RemoteAddr().String(), src.LocalAddr().String(), dest.LocalAddr().String(), dest.RemoteAddr().String())
	_, _ = io.Copy(src, dest)
	log.Printf("[-] [%s->%s] closed.\n", src.RemoteAddr().String(), src.LocalAddr().String())
	wg.Done()
}

func listener(listenPort0, listenPort1 string) {
	ln0 := listen(listenPort0)
	ln1 := listen(listenPort1)
	log.Printf("[*] listen port on: [%s %s],[%s %s]\n", ln0.Addr().Network(), ln0.Addr().String(), ln1.Addr().Network(), ln1.Addr().String())

	for true {
		cc := make(chan net.Conn, 2)

		go accept(cc, ln0)
		go accept(cc, ln1)

		conn0, conn1 := <-cc, <-cc
		mutualCopyIO(conn0, conn1)
	}
}

func forward(listenPort string, targetAddress string) {
	ln := listen(listenPort)
	log.Printf("[*] listen on: [%s %s] forward to: [%s]\n", ln.Addr().Network(), ln.Addr().String(), targetAddress)
	for true {
		cc := make(chan net.Conn, 2)

		go accept(cc, ln)
		conn0 := <-cc

		go dial(cc, targetAddress)
		conn1 := <-cc
		go mutualCopyIO(conn0, conn1)
	}
}

func agent(targetAddress0 string, targetAddress1 string) {
	log.Printf("[*] agent with: [%s %s]\n", targetAddress0, targetAddress1)
	for {
		cc := make(chan net.Conn, 2)

		go dial(cc, targetAddress0)
		go dial(cc, targetAddress1)

		conn0, conn1 := <-cc, <-cc
		mutualCopyIO(conn0, conn1)
	}
}

func mutualCopyIO(conn0, conn1 net.Conn) {
	wg := sync.WaitGroup{}
	wg.Add(2)
	go copyIO(conn0, conn1, &wg)
	go copyIO(conn1, conn0, &wg)
	wg.Wait()
}

func dial(cc chan net.Conn, targetAddress string) {
	for true {
		conn, err := net.DialTimeout("tcp", targetAddress, time.Duration(Timeout)*time.Second)
		if err != nil {
			log.Printf("[x] connect [%s] error [%s]\n", targetAddress, err.Error())
			log.Printf("[*] retry to connect: [%s] after [%d] second\n", targetAddress, RetryInterval)
			time.Sleep(time.Duration(RetryInterval) * time.Second)
			continue
		}
		log.Printf("[+] [%s->%s] connected to target\n", conn.LocalAddr().String(), targetAddress)
		cc <- conn
		break
	}
}

func accept(cc chan net.Conn, ln net.Listener) {
	for true {
		log.Printf("[*] waiting for client to connect [%s %s]\n", ln.Addr().Network(), ln.Addr().String())
		c, err := ln.Accept()
		if err != nil {
			log.Printf("[x] accept error [%s]\n", err.Error())
			continue
		}
		log.Printf("[+] [%s<-%s] new client connected\n", c.LocalAddr().String(), c.RemoteAddr().String())
		cc <- c
		break
	}
}

func listen(listenPort string) net.Listener {
	if !strings.Contains(listenPort, ":") {
		listenPort = "0.0.0.0:" + listenPort
	}
	ln, err := net.Listen("tcp", listenPort)
	if err != nil {
		log.Printf("[x] listen error [%s]\n", err.Error())
		os.Exit(0)
	}
	return ln
}

func proxy(protocol, listenAddress string, args []string) {
	if protocol != "http" && protocol != "https" {
		log.Fatal("protocol must be either http or https")
	}

	if !strings.Contains(listenAddress, ":") {
		listenAddress = "0.0.0.0:" + listenAddress
	}

	server := &http.Server{
		Addr: listenAddress,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodConnect {
				handleTunneling(w, r)
			} else {
				handleHttp(w, r)
			}
		}),
		TLSNextProto: make(map[string]func(*http.Server, *tls.Conn, http.Handler)),
	}

	if protocol == "https" {
		if len(args) < 6 {
			printHelp()
			os.Exit(0)
		}
		certFile := args[5]
		keyFile := args[6]
		log.Printf("[*] https proxy listen on: [%s] with cert: [%s] and key: [%s]", listenAddress, certFile, keyFile)
		log.Fatal(server.ListenAndServeTLS(certFile, keyFile))
	} else {
		log.Printf("[*] http proxy listen on: [%s]", listenAddress)
		log.Fatal(server.ListenAndServe())
	}
}

func handleTunneling(w http.ResponseWriter, r *http.Request) {
	remoteConn, err := net.DialTimeout("tcp", r.Host, time.Duration(Timeout)*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}
	centralConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}

	go mutualCopyIO(remoteConn, centralConn)
}

func handleHttp(w http.ResponseWriter, req *http.Request) {
	resp, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	defer resp.Body.Close()

	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}
