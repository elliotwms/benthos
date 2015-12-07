/*
Copyright (c) 2014 Ashley Jeffs

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/

package main

import (
	"errors"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pebbe/zmq4"
)

//--------------------------------------------------------------------------------------------------

func main() {
	var address string
	flag.StringVar(&address, "addr", "tcp://localhost:1234", "Address of the benthos server")

	flag.Parse()

	fmt.Println("This is a benchmarking utility for benthos.")
	fmt.Println("Make sure you are running benthos with the ./test/zmq.yaml config.")

	if len(flag.Args()) != 2 {
		fmt.Printf("\nUsage: %v <interval> <blob_size>\n", os.Args[0])
		fmt.Printf("e.g.: %v 10us 1024\n", os.Args[0])
		return
	}

	interval, err := time.ParseDuration(flag.Args()[0])
	if err != nil {
		panic(err)
	}

	blobSize, err := strconv.Atoi(flag.Args()[1])
	if err != nil {
		panic(err)
	}

	ctx, err := zmq4.NewContext()
	if nil != err {
		panic(err)
	}

	pushSocket, err := ctx.NewSocket(zmq4.PUSH)
	if nil != err {
		panic(err)
	}
	pushSocket.Connect(address)

	wg := sync.WaitGroup{}
	wg.Add(1)

	dataBlob := make([]byte, blobSize)
	for i := range dataBlob {
		dataBlob[i] = byte(rand.Int())
	}

	var running int32 = 1

	go func() {
		for atomic.LoadInt32(&running) == 1 {
			<-time.After(interval)
			nowBytes, err := time.Now().MarshalBinary()
			if err != nil {
				panic(err)
			}

			_, err = pushSocket.SendMessage(nowBytes, dataBlob)
			if err != nil {
				panic(err)
			}
		}
		wg.Done()
	}()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	// Wait for termination signal
	select {
	case <-sigChan:
	}

	atomic.StoreInt32(&running, 0)
	go func() {
		<-time.After(time.Second * 10)
		panic(errors.New("Timed out waiting for processes to end"))
	}()
	wg.Wait()
}

//--------------------------------------------------------------------------------------------------
