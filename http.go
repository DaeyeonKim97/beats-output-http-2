package http

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/DaeyeonKim97/beats-output-http-2/resolver"
	"github.com/elastic/beats/v7/libbeat/beat"
	"github.com/elastic/beats/v7/libbeat/common"
	"github.com/elastic/beats/v7/libbeat/logp"
	"github.com/elastic/beats/v7/libbeat/outputs"
	"github.com/elastic/beats/v7/libbeat/outputs/codec"
	"github.com/elastic/beats/v7/libbeat/publisher"
	"github.com/json-iterator/go"
	"io/ioutil"
	"net"
	"net/http"
	"sync"
	"time"
	"strings"
)

var json = jsoniter.ConfigCompatibleWithStandardLibrary

var dnsCache = resolver.NewDNSResolver()

func init() {
	outputs.RegisterType("http", makeHTTP)
}

type httpOutput struct {
	log       *logp.Logger
	beat      beat.Info
	observer  outputs.Observer
	codec     codec.Codec
	client    *http.Client
	serialize func(event *publisher.Event) ([]byte, error)
	reqPool   sync.Pool
	conf      config
}

// makeHTTP instantiates a new http output instance.
func makeHTTP(
	_ outputs.IndexManager,
	beat beat.Info,
	observer outputs.Observer,
	cfg *common.Config,
) (outputs.Group, error) {

	config := defaultConfig
	if err := cfg.Unpack(&config); err != nil {
		return outputs.Fail(err)
	}

	ho := &httpOutput{
		log:      logp.NewLogger("http"),
		beat:     beat,
		observer: observer,
		conf:     config,
	}

	// disable bulk support in publisher pipeline
	if err := cfg.SetInt("bulk_max_size", -1, -1); err != nil {
		ho.log.Error("Disable bulk error: ", err)
	}

	//select serializer
	ho.serialize = ho.serializeAll

	if config.OnlyFields {
		ho.serialize = ho.serializeOnlyFields
	}

	// init output
	if err := ho.init(beat, config); err != nil {
		return outputs.Fail(err)
	}

	return outputs.Success(-1, config.MaxRetries, ho)
}

func (out *httpOutput) init(beat beat.Info, c config) error {
	var err error

	out.codec, err = codec.CreateEncoder(beat, c.Codec)
	if err != nil {
		return err
	}

	tr := &http.Transport{
		MaxIdleConns:          out.conf.MaxIdleConns,
		ResponseHeaderTimeout: time.Duration(out.conf.ResponseHeaderTimeout) * time.Millisecond,
		IdleConnTimeout:       time.Duration(out.conf.IdleConnTimeout) * time.Second,
		DisableCompression:    !out.conf.Compression,
		DisableKeepAlives:     !out.conf.KeepAlive,
		DialContext: func(ctx context.Context, network string, addr string) (conn net.Conn, err error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := dnsCache.LookupHost(ctx, host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				var dialer net.Dialer
				conn, err = dialer.DialContext(ctx, network, net.JoinHostPort(ip, port))
				if err == nil {
					break
				}
			}
			return
		},
	}

	out.client = &http.Client{
		Transport: tr,
	}

	out.reqPool = sync.Pool{
		New: func() interface{} {
			req, err := http.NewRequest("POST", out.conf.URL, nil)
			if err != nil {
				return err
			}
			return req
		},
	}

	out.log.Infof("Initialized http output:\n"+
		"url=%v\n"+
		"codec=%v\n"+
		"only_fields=%v\n"+
		"max_retries=%v\n"+
		"compression=%v\n"+
		"keep_alive=%v\n"+
		"max_idle_conns=%v\n"+
		"idle_conn_timeout=%vs\n"+
		"response_header_timeout=%vms\n"+
		"username=%v\n"+
		"password=%v\n",
		c.URL, c.Codec, c.OnlyFields, c.MaxRetries, c.Compression,
		c.KeepAlive, c.MaxIdleConns, c.IdleConnTimeout, c.ResponseHeaderTimeout,
		c.Username, maskPass(c.Password))
	return nil
}

func maskPass(password string) string {
	result := ""
	if len(password) <= 8 {
		for i := 0; i < len(password); i++ {
			result += "*"
		}
		return result
	}

	for i, char := range password {
		if i > 1 && i < len(password)-2 {
			result += "*"
		} else {
			result += string(char)
		}
	}

	return result
}

// Implement Client
func (out *httpOutput) Close() error {
	out.client.CloseIdleConnections()
	return nil
}

func (out *httpOutput) serializeOnlyFields(event *publisher.Event) ([]byte, error) {
	fields := event.Content.Fields
	fields["@timestamp"] = event.Content.Timestamp
	for key, val := range out.conf.AddFields {
		fields[key] = val
	}
	body, err := fields.GetValue("body")

	slice := strings.Split(body.(string), " ")
	
	if err != nil {
		out.log.Error("slice error: ", err)
		return make([]byte, 0), err
	}

	fields["ifindex"] = slice[3]
	fields["actionCode"] = slice[4]
	fields["aclTag"] = slice[6]
	fields["ruleDesc"] = slice[8]
	fields["protocol"] = slice[9]
	fields["NFFOrDash"] = slice[10]
	fields["srcIp"] = slice[11]
	fields["srcPort"] = slice[12]
	fields["dstIp"] = slice[13]
	fields["dstPort"] = slice[14]
	fields["octProto"] = slice[17]
	fields["isInput"] = slice[19]
	fields["isSlowpath"] = slice[21]
	fields["hexFlegs"] = slice[23]
	fields["invalidOrDash"] = slice[25]
	fields["tcpflags"] = slice[26]
	fields["rsvd"] = slice[28]

	if len(slice) > 29{
		fields["dur"] = slice[30]
		fields["pkts"] = slice[32]
		fields["bytes"] = slice[34]
	}


	serializedEvent, err := json.Marshal(&fields)
	if err != nil {
		out.log.Error("Serialization error: ", err)
		return make([]byte, 0), err
	}
	return serializedEvent, nil
}

func (out *httpOutput) serializeAll(event *publisher.Event) ([]byte, error) {
	serializedEvent, err := out.codec.Encode(out.beat.Beat, &event.Content)
	if err != nil {
		out.log.Error("Serialization error: ", err)
		return make([]byte, 0), err
	}
	return serializedEvent, nil
}

func (out *httpOutput) Publish(_ context.Context, batch publisher.Batch) error {
	st := out.observer
	events := batch.Events()
	st.NewBatch(len(events))

	if len(events) == 0 {
		batch.ACK()
		return nil
	}

	for i := range events {
		event := events[i]

		serializedEvent, err := out.serialize(&event)

		if err != nil {
			if event.Guaranteed() {
				out.log.Errorf("Failed to serialize the event: %+v", err)
			} else {
				out.log.Warnf("Failed to serialize the event: %+v", err)
			}
			out.log.Debugf("Failed event: %v", event)

			batch.RetryEvents(events)
			st.Failed(len(events))
			return nil
		}

		if err = out.send(serializedEvent); err != nil {
			if event.Guaranteed() {
				out.log.Errorf("Writing event to http failed with: %+v", err)
			} else {
				out.log.Warnf("Writing event to http failed with: %+v", err)
			}

			//batch.RetryEvents(events)
			st.Failed(len(events))
			return nil
		}
	}

	batch.ACK()
	st.Acked(len(events))
	return nil
}

func (out *httpOutput) String() string {
	return "http(" + out.conf.URL + ")"
}

func (out *httpOutput) send(data []byte) error {

	req, err := out.getReq(data)
	if err != nil {
		return err
	}
	defer out.putReq(req)

	resp, err := out.client.Do(req)
	if err != nil {
		return err
	}

	err = resp.Body.Close()
	if err != nil {
		out.log.Warn("Close response body error:", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bad response code: %d", resp.StatusCode)
	}

	return nil
}

func (out *httpOutput) getReq(data []byte) (*http.Request, error) {
	tmp := out.reqPool.Get()

	req, ok := tmp.(*http.Request)
	if ok {
		buf := bytes.NewBuffer(data)
		req.Body = ioutil.NopCloser(buf)
		req.Header.Set("User-Agent", "beat "+out.beat.Version)
		req.Header.Set("Content-Type", "application/json")
		if out.conf.Username != "" {
			req.SetBasicAuth(out.conf.Username, out.conf.Password)
		}
		return req, nil
	}

	err, ok := tmp.(error)
	if ok {
		return nil, err
	}

	return nil, errors.New("pool assertion error")
}

func (out *httpOutput) putReq(req *http.Request) {
	out.reqPool.Put(req)
}
