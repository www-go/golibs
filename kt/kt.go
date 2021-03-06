package kt

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"
)

const DEFAULT_TIMEOUT = 2 * time.Second

// Conn represents a connection to a kyoto tycoon endpoint.
// It uses a connection pool to efficiently communicate with the server.
// Conn is safe for concurrent use.
type Conn struct {
	// Has to be first for atomic alignment
	retryCount uint64

	timeout   time.Duration
	host      string
	transport *http.Transport
}

// KT has 2 interfaces, A restful one and an RPC one.
// The RESTful interface is usually much faster than
// the RPC one, but not all methods are implemented.
// Use the RESTFUL interfaces when we can and fallback
// to the RPC one when needed.
//
// The RPC format uses tab separated values with a choice of encoding
// for each of the fields. We use base64 since it is always safe.
//
// REST format is just the body of the HTTP request being the value.

// NewConn creates a connection to an Kyoto Tycoon endpoint.
func NewConn(host string, port int, poolsize int, timeout time.Duration) (*Conn, error) {
	portstr := strconv.Itoa(port)
	c := &Conn{
		timeout: timeout,
		host:    net.JoinHostPort(host, portstr),
		transport: &http.Transport{
			ResponseHeaderTimeout: timeout,
			MaxIdleConnsPerHost:   poolsize,
		},
	}

	// connectivity check so that we can bail out
	// early instead of when we do the first operation.
	_, _, err := c.doRPC("/rpc/void", nil)
	if err != nil {
		return nil, err
	}
	return c, nil
}

var (
	ErrTimeout = errors.New("kt: operation timeout")
	// the wording on this error is deliberately weird,
	// because users would search for the string logical inconsistency
	// in order to find lookup misses.
	ErrNotFound = errors.New("kt: entry not found aka logical inconsistency")
	// old gokabinet returned this error on success. Keeping around "for compatibility" until
	// I can kill it with fire.
	ErrSuccess = errors.New("kt: success")
)

// RetryCount is the number of retries performed due to the remote end
// closing idle connections.
//
// The value increases monotonically, until it wraps to 0.
func (c *Conn) RetryCount() uint64 {
	return atomic.LoadUint64(&c.retryCount)
}

// Count returns the number of records in the database
func (c *Conn) Count() (int, error) {
	code, m, err := c.doRPC("/rpc/status", nil)
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, makeError(m)
	}
	return strconv.Atoi(string(findRec(m, "count").Value))
}

// Remove deletes the data at key in the database.
func (c *Conn) Remove(key string) error {
	code, body, err := c.doREST("DELETE", key, nil)
	if err != nil {
		return err
	}
	if code == 404 {
		return ErrNotFound
	}
	if code != 204 {
		return errors.New(string(body))
	}
	return nil
}

// GetBulk retrieves the keys in the map. The results will be filled in on function return.
// If a key was not found in the database, it will be removed from the map.
func (c *Conn) GetBulk(keysAndVals map[string]string) error {
	m := make(map[string][]byte)
	for k := range keysAndVals {
		m[k] = zeroslice
	}
	err := c.GetBulkBytes(m)
	if err != nil {
		return err
	}
	for k := range keysAndVals {
		b, ok := m[k]
		if ok {
			keysAndVals[k] = string(b)
		} else {
			delete(keysAndVals, k)
		}
	}
	return nil
}

// Get retrieves the data stored at key. ErrNotFound is
// returned if no such data exists
func (c *Conn) Get(key string) (string, error) {
	s, err := c.GetBytes(key)
	if err != nil {
		return "", err
	}
	return string(s), nil
}

// GetBytes retrieves the data stored at key in the format of a byte slice
// ErrNotFound is returned if no such data is found.
func (c *Conn) GetBytes(key string) ([]byte, error) {
	code, body, err := c.doREST("GET", key, nil)
	if err != nil {
		return nil, err
	}
	switch code {
	case 200:
		break
	case 404:
		return nil, ErrNotFound
	default:
		return nil, errors.New(string(body))
	}
	return body, nil

}

// Set stores the data at key
func (c *Conn) Set(key string, value []byte) error {
	code, body, err := c.doREST("PUT", key, value)
	if err != nil {
		return err
	}
	if code != 201 {
		return errors.New(string(body))
	}

	return nil
}

var zeroslice = []byte("0")

// GetBulkBytes retrieves the keys in the map. The results will be filled in on function return.
// If a key was not found in the database, it will be removed from the map.
func (c *Conn) GetBulkBytes(keys map[string][]byte) error {

	// The format for querying multiple keys in KT is to send a
	// TSV value for each key with a _ as a prefix.
	// KT then returns the value as a TSV set with _ in front of the keys
	keystransmit := make([]KV, 0, len(keys))
	for k, _ := range keys {
		// we set the value to nil because we want a sentinel value
		// for when no data was found. This is important for
		// when we remove the not found keys from the map
		keys[k] = nil
		keystransmit = append(keystransmit, KV{"_" + k, zeroslice})
	}
	code, m, err := c.doRPC("/rpc/get_bulk", keystransmit)
	if err != nil {
		return err
	}
	if code != 200 {
		return makeError(m)
	}
	for _, kv := range m {
		if kv.Key[0] != '_' {
			continue
		}
		keys[kv.Key[1:]] = kv.Value
	}
	for k, v := range keys {
		if v == nil {
			delete(keys, k)
		}
	}
	return nil
}

// SetBulk stores the values in the map.
func (c *Conn) SetBulk(values map[string]string) (int64, error) {
	vals := make([]KV, 0, len(values))
	for k, v := range values {
		vals = append(vals, KV{"_" + k, []byte(v)})
	}
	code, m, err := c.doRPC("/rpc/set_bulk", vals)
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, makeError(m)
	}
	return strconv.ParseInt(string(findRec(m, "num").Value), 10, 64)
}

// RemoveBulk deletes the values
func (c *Conn) RemoveBulk(keys []string) (int64, error) {
	vals := make([]KV, 0, len(keys))
	for _, k := range keys {
		vals = append(vals, KV{"_" + k, zeroslice})
	}
	code, m, err := c.doRPC("/rpc/remove_bulk", vals)
	if err != nil {
		return 0, err
	}
	if code != 200 {
		return 0, makeError(m)
	}
	return strconv.ParseInt(string(findRec(m, "num").Value), 10, 64)
}

// MatchPrefix performs the match_prefix operation against the server
// It returns a sorted list of strings.
// The error may be ErrSuccess in the case that no records were found.
// This is for compatibility with the old gokabinet library.
func (c *Conn) MatchPrefix(key string, maxrecords int64) ([]string, error) {
	keystransmit := []KV{
		{"prefix", []byte(key)},
		{"max", []byte(strconv.FormatInt(maxrecords, 10))},
	}
	code, m, err := c.doRPC("/rpc/match_prefix", keystransmit)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		return nil, makeError(m)
	}
	res := make([]string, 0, len(m))
	for _, kv := range m {
		if kv.Key[0] == '_' {
			res = append(res, string(kv.Key[1:]))
		}
	}
	if len(res) == 0 {
		// yeah, gokabinet was weird here.
		return nil, ErrSuccess
	}
	return res, nil
}

var base64headers http.Header
var identityheaders http.Header

func init() {
	identityheaders = make(http.Header)
	identityheaders.Set("Content-Type", "text/tab-separated-values")
	base64headers = make(http.Header)
	base64headers.Set("Content-Type", "text/tab-separated-values; colenc=B")
}

// KV uses an explicit structure here rather than a map[string][]byte
// because we need ordered data.
type KV struct {
	Key   string
	Value []byte
}

// Do an RPC call against the KT endpoint.
func (c *Conn) doRPC(path string, values []KV) (code int, vals []KV, err error) {
	url := &url.URL{
		Scheme: "http",
		Host:   c.host,
		Path:   path,
	}
	body, enc := TSVEncode(values)
	headers := identityheaders
	if enc == Base64Enc {
		headers = base64headers
	}
	resp, t, err := c.roundTrip("POST", url, headers, body)
	if err != nil {
		return 0, nil, err
	}
	resultBody, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if !t.Stop() {
		return 0, nil, ErrTimeout
	}
	if err != nil {
		return 0, nil, err
	}
	m, err := DecodeValues(resultBody, resp.Header.Get("Content-Type"))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, m, nil
}

func (c *Conn) roundTrip(method string, url *url.URL, headers http.Header, body []byte) (*http.Response, *time.Timer, error) {
	req, t := c.makeRequest(method, url, headers, body)
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		// Ideally we would only retry when we hit a network error. This doesn't work
		// since net/http wraps some of these errors. Do the simple thing and retry eagerly.
		t.Stop()
		c.transport.CloseIdleConnections()
		req, t = c.makeRequest(method, url, headers, body)
		resp, err = c.transport.RoundTrip(req)
		atomic.AddUint64(&c.retryCount, 1)
	}
	if err != nil {
		if !t.Stop() {
			err = ErrTimeout
		}
		return nil, nil, err
	}
	return resp, t, nil
}

func (c *Conn) makeRequest(method string, url *url.URL, headers http.Header, body []byte) (*http.Request, *time.Timer) {
	var rc io.ReadCloser
	if body != nil {
		rc = ioutil.NopCloser(bytes.NewReader(body))
	}
	req := &http.Request{
		Method:        method,
		URL:           url,
		Header:        headers,
		Body:          rc,
		ContentLength: int64(len(body)),
	}
	t := time.AfterFunc(c.timeout, func() {
		c.transport.CancelRequest(req)
	})
	return req, t
}

type Encoding int

const (
	IdentityEnc Encoding = iota
	Base64Enc
)

// Encode the request body in TSV. The encoding is chosen based
// on whether there are any binary data in the key/values
func TSVEncode(values []KV) ([]byte, Encoding) {
	var bufsize int
	var hasbinary bool
	for _, kv := range values {
		// length of key
		hasbinary = hasbinary || hasBinary(kv.Key)
		bufsize += base64.StdEncoding.EncodedLen(len(kv.Key))
		// tab
		bufsize += 1
		// value
		hasbinary = hasbinary || hasBinarySlice(kv.Value)
		bufsize += base64.StdEncoding.EncodedLen(len(kv.Value))
		// newline
		bufsize += 1
	}
	buf := make([]byte, bufsize)
	var n int
	for _, kv := range values {
		if hasbinary {
			base64.StdEncoding.Encode(buf[n:], []byte(kv.Key))
			n += base64.StdEncoding.EncodedLen(len(kv.Key))
		} else {
			n += copy(buf[n:], kv.Key)
		}
		buf[n] = '\t'
		n++
		if hasbinary {
			base64.StdEncoding.Encode(buf[n:], kv.Value)
			n += base64.StdEncoding.EncodedLen(len(kv.Value))
		} else {
			n += copy(buf[n:], kv.Value)
		}
		buf[n] = '\n'
		n++
	}
	enc := IdentityEnc
	if hasbinary {
		enc = Base64Enc
	}
	return buf, enc
}

func hasBinary(b string) bool {
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c < 0x20 || c > 0x7e {
			return true
		}
	}
	return false
}

func hasBinarySlice(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			return true
		}
	}
	return false
}

// DecodeValues takes a response from an KT RPC call decodes it into a list of key
// value pairs.
func DecodeValues(buf []byte, contenttype string) ([]KV, error) {
	if len(buf) == 0 {
		return nil, nil
	}
	// Ideally, we should parse the mime media type here,
	// but this is an expensive operation because mime is just
	// that awful.
	//
	// KT can return values in 3 different formats, Tab separated values (TSV) without any field encoding,
	// TSV with fields base64 encoded or TSV with URL encoding.
	// KT does not give you any option as to the format that it returns, so we have to implement all of them
	//
	// KT responses are pretty simple and we can rely
	// on it putting the parameter of colenc=[BU] at
	// the end of the string. Just look for B, U or s
	// (last character of tab-separated-values)
	// to figure out which field encoding is used.
	var decodef decodefunc
	switch contenttype[len(contenttype)-1] {
	case 'B':
		decodef = base64Decode
	case 'U':
		decodef = urlDecode
	case 's':
		decodef = identityDecode
	default:
		return nil, fmt.Errorf("kt responded with unknown Content-Type: %s", contenttype)
	}

	// Because of the encoding, we can tell how many records there
	// are by scanning through the input and counting the \n's
	var recCount int
	for _, v := range buf {
		if v == '\n' {
			recCount++
		}
	}
	result := make([]KV, 0, recCount)
	b := bytes.NewBuffer(buf)
	for {
		key, err := b.ReadBytes('\t')
		if err != nil {
			return result, nil
		}
		key = decodef(key[:len(key)-1])
		value, err := b.ReadBytes('\n')
		if len(value) > 0 {
			fieldlen := len(value) - 1
			if value[len(value)-1] != '\n' {
				fieldlen = len(value)
			}
			value = decodef(value[:fieldlen])
			result = append(result, KV{string(key), value})
		}
		if err != nil {
			return result, nil
		}
	}
}

// decodefunc takes a byte slice and decodes the
// value in place. It returns a slice pointing into
// the original byte slice. It is used for decoding the
// individual fields of the TSV that kt returns
type decodefunc func([]byte) []byte

// Don't do anything, this is pure TSV
func identityDecode(b []byte) []byte {
	return b
}

// Base64 decode each of the field
func base64Decode(b []byte) []byte {
	n, _ := base64.StdEncoding.Decode(b, b)
	return b[:n]
}

// Decode % escaped URL format
func urlDecode(b []byte) []byte {
	res := b
	resi := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '%' {
			res[resi] = b[i]
			resi++
			continue
		}
		res[resi] = unhex(b[i+1])<<4 | unhex(b[i+2])
		resi++
		i += 2
	}
	return res[:resi]
}

// copied from net/url
func unhex(c byte) byte {
	switch {
	case '0' <= c && c <= '9':
		return c - '0'
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10
	}
	return 0
}

// TODO: make this return errors that can be introspected more easily
// and make it trim components of the error to filter out unused information.
func makeError(m []KV) error {
	kv := findRec(m, "ERROR")
	if kv.Key == "" {
		return errors.New("kt: generic error")
	}
	return errors.New("kt: " + string(kv.Value))
}

func findRec(kvs []KV, key string) KV {
	for _, kv := range kvs {
		if kv.Key == key {
			return kv
		}
	}
	return KV{}
}

// empty header for REST calls.
var emptyHeader = make(http.Header)

func (c *Conn) doREST(op string, key string, val []byte) (code int, body []byte, err error) {
	newkey := urlenc(key)
	url := &url.URL{
		Scheme: "http",
		Host:   c.host,
		Opaque: newkey,
	}
	resp, t, err := c.roundTrip(op, url, emptyHeader, val)
	if err != nil {
		return 0, nil, err
	}
	resultBody, err := ioutil.ReadAll(resp.Body)
	resp.Body.Close()
	if !t.Stop() {
		err = ErrTimeout
	}
	return resp.StatusCode, resultBody, err
}

// encode the key for use in a RESTFUL url
// KT requires that we use URL escaped values for
// anything not safe in a query component.
// Add a slash for the leading slash in the url.
func urlenc(s string) string {
	return "/" + url.QueryEscape(s)
}
