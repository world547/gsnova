package paas

import (
	"bytes"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/yinqiwen/gsnova/common/event"
	"github.com/yinqiwen/gsnova/local/proxy"
)

var paasHttpClient *http.Client

type chunkChannelReadCloser struct {
	chunkChannel chan []byte
	lastread     []byte
}

func (cr *chunkChannelReadCloser) Read(p []byte) (int, error) {
	if len(cr.lastread) == 0 {
		cr.lastread = <-cr.chunkChannel
		if nil == cr.lastread {
			return 0, io.EOF
		}
	}
	if len(cr.lastread) <= len(p) {
		copy(p, cr.lastread)
		n := len(cr.lastread)
		cr.lastread = nil
		return n, nil
	}
	copy(p, cr.lastread[0:len(p)])
	cr.lastread = cr.lastread[len(p):]
	return len(p), nil
}
func (cr *chunkChannelReadCloser) Close() error {
	return nil
}
func (cr *chunkChannelReadCloser) offer(p []byte) {
	cr.chunkChannel <- p
}

func (cr *chunkChannelReadCloser) prepend(p []byte) {
	if len(cr.lastread) > 0 {
		log.Printf("###########################No empty:%d", len(cr.lastread))
	}
	cr.lastread = append(p, cr.lastread...)
	//cr.lastread = p
}

type httpChannel struct {
	addr    string
	idx     int
	pushurl *url.URL
	pullurl *url.URL

	iv        uint64
	rbody     io.ReadCloser
	pulling   bool
	pushing   bool
	chunkChan *chunkChannelReadCloser
}

func (hc *httpChannel) ReadTimeout() time.Duration {
	readTimeout := proxy.GConf.PAAS.HTTPReadTimeout
	if 0 == readTimeout {
		readTimeout = 30
	}
	return time.Duration(readTimeout) * time.Second
}

func (hc *httpChannel) SetIV(iv uint64) {
	//log.Printf("Change IV from %d to %d", hc.iv, iv)
	hc.iv = iv
	if nil != hc.chunkChan && hc.pushing {
		hc.chunkChan.offer(nil)
	}
}

func (hc *httpChannel) Open() error {
	if nil == hc.pushurl {
		u, err := url.Parse(hc.addr)
		if nil != err {
			return err
		}
		u.Path = "/http/push"
		hc.pushurl = u
	}
	if proxy.GConf.PAAS.HTTPChunkPushEnable && nil == hc.chunkChan {
		hc.chunkChan = new(chunkChannelReadCloser)
		hc.chunkChan.chunkChannel = make(chan []byte, 100)
		go hc.chunkPush()
	}
	return hc.pull()
}

func (hc *httpChannel) Closed() bool {
	return nil == hc.rbody
}

func (tc *httpChannel) Request([]byte) ([]byte, error) {
	return nil, nil
}

func (hc *httpChannel) Close() error {
	return nil
}

func (hc *httpChannel) pull() error {
	if nil == hc.pullurl {
		u, err := url.Parse(hc.addr)
		if nil != err {
			return err
		}
		u.Path = "/http/pull"
		hc.pullurl = u
	}
	if hc.pulling {
		return nil
	}
	readAuth := proxy.NewAuthEvent()
	readAuth.Index = int64(hc.idx)
	readAuth.IV = hc.iv
	var buf bytes.Buffer
	event.EncryptEvent(&buf, readAuth, 0)
	hc.pulling = true
	log.Printf("[%s:%d] pull channel start.", hc.addr, hc.idx)
	_, err := hc.postURL(buf.Bytes(), hc.pullurl)
	hc.pulling = false
	return err
}

func (hc *httpChannel) Read(p []byte) (int, error) {
	if hc.rbody == nil && hc.idx >= 0 {
		hc.pull()
	}
	start := time.Now()
	for nil == hc.rbody {
		if time.Now().After(start.Add(hc.ReadTimeout())) {
			return 0, proxy.ErrChannelReadTimeout
		}
		time.Sleep(1 * time.Millisecond)
	}
	n, err := hc.rbody.Read(p)
	if nil != err {
		hc.rbody.Close()
		hc.rbody = nil
	}
	return n, err
}

func buildHTTPReq(u *url.URL, body io.ReadCloser) *http.Request {
	req := &http.Request{
		Method:     "POST",
		URL:        u,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       u.Host,
		Header:     make(http.Header),
		Body:       body,
	}
	req.Close = false
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Type", "image/jpeg")
	if len(proxy.GConf.UserAgent) > 0 {
		req.Header.Set("User-Agent", proxy.GConf.UserAgent)
	}
	return req
}

func (hc *httpChannel) chunkPush() {
	u := hc.pushurl
	var ticker *time.Ticker
	closePush := func() {
		//ticker := time.NewTicker(10 * time.Second)
		select {
		case <-ticker.C:
			if hc.pushing {
				//force push channel close
				hc.chunkChan.offer(nil)
			}
		}
	}

	for {
		if len(hc.chunkChan.chunkChannel) == 0 {
			time.Sleep(10 * time.Millisecond)
			continue
		}
		hc.pushing = true
		req := buildHTTPReq(u, hc.chunkChan)
		req.ContentLength = -1
		wAuth := proxy.NewAuthEvent()
		wAuth.Index = int64(hc.idx)
		wAuth.IV = hc.iv
		var buf bytes.Buffer
		event.EncryptEvent(&buf, wAuth, 0)
		hc.chunkChan.prepend(buf.Bytes())
		period := proxy.GConf.PAAS.HTTPReconnectPeriod
		if period == 0 {
			period = 30
		}
		ticker = time.NewTicker(time.Duration(period) * time.Second)
		go closePush()
		log.Printf("[%s:%d] chunk push channel start.", hc.addr, hc.idx)
		response, err := paasHttpClient.Do(req)
		if nil != err || response.StatusCode != 200 {
			log.Printf("Failed to write data to PAAS:%s for reason:%v or res:%v", u.String(), err, response)
		} else {
			log.Printf("[%s:%d] chunk push channel stop.", hc.addr, hc.idx)
		}
		ticker.Stop()
		hc.pushing = false
	}

}

func (hc *httpChannel) postURL(p []byte, u *url.URL) (n int, err error) {
	req := buildHTTPReq(u, ioutil.NopCloser(bytes.NewBuffer(p)))
	req.ContentLength = int64(len(p))
	if u == hc.pullurl {
		period := proxy.GConf.PAAS.HTTPReconnectPeriod
		if 0 == period {
			period = 30
		}
		req.Header.Set("X-PullPeriod", strconv.Itoa(period))
	}
	response, err := paasHttpClient.Do(req)
	if nil != err || response.StatusCode != 200 { //try once more
		req.Body = ioutil.NopCloser(bytes.NewBuffer(p))
		response, err = paasHttpClient.Do(req)
	}
	if nil != err || response.StatusCode != 200 {
		log.Printf("Failed to write data to PAAS:%s for reason:%v or res:%v", u.String(), err, response)
		return 0, err
	}
	if response.ContentLength != 0 && nil != response.Body {
		hc.rbody = response.Body
	}
	return len(p), nil
}

func (hc *httpChannel) Write(p []byte) (n int, err error) {
	if nil != hc.chunkChan {
		pp := make([]byte, len(p))
		copy(pp, p)
		hc.chunkChan.offer(pp)
		return len(p), nil
	}
	return hc.postURL(p, hc.pushurl)
}

func newHTTPChannel(addr string, idx int) (*proxy.RemoteChannel, error) {

	rc := &proxy.RemoteChannel{
		Addr:          addr,
		Index:         idx,
		DirectIO:      false,
		OpenJoinAuth:  false,
		WriteJoinAuth: !proxy.GConf.PAAS.HTTPChunkPushEnable,
	}

	tc := new(httpChannel)
	tc.addr = addr
	tc.idx = idx
	rc.C = tc

	err := rc.Init()
	if nil != err {
		return nil, err
	}
	return rc, nil
}
