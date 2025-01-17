//go:build !windows
// +build !windows

package main

/*
  This code is currently only intended to be used by ostree
  to fetch content via containers.  The API is subject
  to change.  A goal however is to stabilize the API
  eventually as a full out-of-process interface to the
  core containers/image library functionality.

  To use this command, in a parent process create a
  `socketpair()` of type `SOCK_SEQPACKET`.  Fork
  off this command, and pass one half of the socket
  pair to the child.  Providing it on stdin (fd 0)
  is the expected default.

  The protocol is JSON for the control layer,
  and  a read side of a `pipe()` passed for large data.

 Base JSON protocol:

 request: { method: "MethodName": args: [arguments] }
 reply: { success: bool, value: JSVAL, pipeid: number, error: string }

 For any non-metadata i.e. payload data from `GetManifest`
 and `GetBlob` the server will pass back the read half of a `pipe(2)` via FD passing,
 along with a `pipeid` integer.

 The expected flow looks like this:

  - Initialize
    And validate the returned protocol version versus
	what your client supports.
  - OpenImage docker://quay.io/someorg/example:latest
    (returns an imageid)
  - GetManifest imageid (and associated <pipeid>)
  (Streaming read data from pipe)
  - FinishPipe <pipeid>
  - GetBlob imageid sha256:...
  (Streaming read data from pipe)
  - FinishPipe <pipeid>
  - GetBlob imageid sha256:...
  (Streaming read data from pipe)
  - FinishPipe <pipeid>
  - CloseImage imageid

 You may interleave invocations of these methods, e.g. one
 can also invoke `OpenImage` multiple times, as well as
 starting multiple GetBlob requests before calling `FinishPipe`
 on them.  The server will stream data into the pipefd
 until `FinishPipe` is invoked.

 Note that the pipe will not be closed by the server until
 the client has invoked `FinishPipe`.  This is to ensure
 that the client checks for errors.  For example, `GetBlob`
 performs digest (e.g. sha256) verification and this must
 be checked after all data has been written.
*/

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"syscall"

	"github.com/containers/image/v5/image"
	"github.com/containers/image/v5/manifest"
	"github.com/containers/image/v5/pkg/blobinfocache"
	"github.com/containers/image/v5/transports/alltransports"
	"github.com/containers/image/v5/types"
	"github.com/opencontainers/go-digest"
	imgspecv1 "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/spf13/cobra"
)

// protocolVersion is semantic version of the protocol used by this proxy.
// The first version of the protocol has major version 0.2 to signify a
// departure from the original code which used HTTP.  The minor version is 1
// instead of 0 to help exercise semver parsers.
const protocolVersion = "0.2.1"

// maxMsgSize is the current limit on a packet size.
// Note that all non-metadata (i.e. payload data) is sent over a pipe.
const maxMsgSize = 32 * 1024

// maxJSONFloat is ECMA Number.MAX_SAFE_INTEGER
// https://developer.mozilla.org/en-US/docs/Web/JavaScript/Reference/Global_Objects/Number/MAX_SAFE_INTEGER
// We hard error if the input JSON numbers we expect to be
// integers are above this.
const maxJSONFloat = float64(1<<53 - 1)

// request is the JSON serialization of a function call
type request struct {
	// Method is the name of the function
	Method string `json:"method"`
	// Args is the arguments (parsed inside the fuction)
	Args []interface{} `json:"args"`
}

// reply is serialized to JSON as the return value from a function call.
type reply struct {
	// Success is true if and only if the call succeeded.
	Success bool `json:"success"`
	// Value is an arbitrary value (or values, as array/map) returned from the call.
	Value interface{} `json:"value"`
	// PipeID is an index into open pipes, and should be passed to FinishPipe
	PipeID uint32 `json:"pipeid"`
	// Error should be non-empty if Success == false
	Error string `json:"error"`
}

// replyBuf is our internal deserialization of reply plus optional fd
type replyBuf struct {
	// value will be converted to a reply Value
	value interface{}
	// fd is the read half of a pipe, passed back to the client
	fd *os.File
	// pipeid will be provided to the client as PipeID, an index into our open pipes
	pipeid uint32
}

// activePipe is an open pipe to the client.
// It contains an error value
type activePipe struct {
	// w is the write half of the pipe
	w *os.File
	// wg is completed when our worker goroutine is done
	wg sync.WaitGroup
	// err may be set in our worker goroutine
	err error
}

// openImage is an opened image reference
type openImage struct {
	// id is an opaque integer handle
	id  uint32
	src types.ImageSource
	img types.Image
}

// proxyHandler is the state associated with our socket.
type proxyHandler struct {
	// lock protects everything else in this structure.
	lock sync.Mutex
	// opts is CLI options
	opts   *proxyOptions
	sysctx *types.SystemContext
	cache  types.BlobInfoCache

	// imageSerial is a counter for open images
	imageSerial uint32
	// images holds our opened images
	images map[uint32]*openImage
	// activePipes maps from "pipeid" to a pipe + goroutine pair
	activePipes map[uint32]*activePipe
}

// Initialize performs one-time initialization, and returns the protocol version
func (h *proxyHandler) Initialize(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var ret replyBuf

	if len(args) != 0 {
		return ret, fmt.Errorf("invalid request, expecting zero arguments")
	}

	if h.sysctx != nil {
		return ret, fmt.Errorf("already initialized")
	}

	sysctx, err := h.opts.imageOpts.newSystemContext()
	if err != nil {
		return ret, err
	}
	h.sysctx = sysctx
	h.cache = blobinfocache.DefaultCache(sysctx)

	r := replyBuf{
		value: protocolVersion,
	}
	return r, nil
}

// OpenImage accepts a string image reference i.e. TRANSPORT:REF - like `skopeo copy`.
// The return value is an opaque integer handle.
func (h *proxyHandler) OpenImage(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()
	var ret replyBuf

	if h.sysctx == nil {
		return ret, fmt.Errorf("Must invoke Initialize")
	}
	if len(args) != 1 {
		return ret, fmt.Errorf("invalid request, expecting one argument")
	}
	imageref, ok := args[0].(string)
	if !ok {
		return ret, fmt.Errorf("Expecting string imageref, not %T", args[0])
	}

	imgRef, err := alltransports.ParseImageName(imageref)
	if err != nil {
		return ret, err
	}
	imgsrc, err := imgRef.NewImageSource(context.Background(), h.sysctx)
	if err != nil {
		return ret, err
	}
	img, err := image.FromUnparsedImage(context.Background(), h.sysctx, image.UnparsedInstance(imgsrc, nil))
	if err != nil {
		return ret, fmt.Errorf("failed to load image: %w", err)
	}

	h.imageSerial++
	openimg := &openImage{
		id:  h.imageSerial,
		src: imgsrc,
		img: img,
	}
	h.images[openimg.id] = openimg
	ret.value = openimg.id

	return ret, nil
}

func (h *proxyHandler) CloseImage(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()
	var ret replyBuf

	if h.sysctx == nil {
		return ret, fmt.Errorf("Must invoke Initialize")
	}
	if len(args) != 1 {
		return ret, fmt.Errorf("invalid request, expecting one argument")
	}
	imgref, err := h.parseImageFromID(args[0])
	if err != nil {
		return ret, err
	}
	imgref.src.Close()
	delete(h.images, imgref.id)

	return ret, nil
}

func parseImageID(v interface{}) (uint32, error) {
	imgidf, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("Expecting integer imageid, not %T", v)
	}
	return uint32(imgidf), nil
}

// parseUint64 validates that a number fits inside a JavaScript safe integer
func parseUint64(v interface{}) (uint64, error) {
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("Expecting numeric, not %T", v)
	}
	if f > maxJSONFloat {
		return 0, fmt.Errorf("Out of range integer for numeric %f", f)
	}
	return uint64(f), nil
}

func (h *proxyHandler) parseImageFromID(v interface{}) (*openImage, error) {
	imgid, err := parseImageID(v)
	if err != nil {
		return nil, err
	}
	imgref, ok := h.images[imgid]
	if !ok {
		return nil, fmt.Errorf("No image %v", imgid)
	}
	return imgref, nil
}

func (h *proxyHandler) allocPipe() (*os.File, *activePipe, error) {
	piper, pipew, err := os.Pipe()
	if err != nil {
		return nil, nil, err
	}
	f := activePipe{
		w: pipew,
	}
	h.activePipes[uint32(pipew.Fd())] = &f
	f.wg.Add(1)
	return piper, &f, nil
}

// GetManifest returns a copy of the manifest, converted to OCI format, along with the original digest.
func (h *proxyHandler) GetManifest(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var ret replyBuf

	if h.sysctx == nil {
		return ret, fmt.Errorf("Must invoke Initialize")
	}
	if len(args) != 1 {
		return ret, fmt.Errorf("invalid request, expecting one argument")
	}
	imgref, err := h.parseImageFromID(args[0])
	if err != nil {
		return ret, err
	}

	ctx := context.TODO()
	rawManifest, manifestType, err := imgref.img.Manifest(ctx)
	if err != nil {
		return ret, err
	}
	// We only support OCI and docker2schema2.  We know docker2schema2 can be easily+cheaply
	// converted into OCI, so consumers only need to see OCI.
	switch manifestType {
	case imgspecv1.MediaTypeImageManifest, manifest.DockerV2Schema2MediaType:
		break
	// Explicitly reject e.g. docker schema 1 type with a "legacy" note
	case manifest.DockerV2Schema1MediaType, manifest.DockerV2Schema1SignedMediaType:
		return ret, fmt.Errorf("Unsupported legacy manifest MIME type: %s", manifestType)
	default:
		return ret, fmt.Errorf("Unsupported manifest MIME type: %s", manifestType)
	}

	// We always return the original digest, as that's what clients need to do pull-by-digest
	// and in general identify the image.
	digest, err := manifest.Digest(rawManifest)
	if err != nil {
		return ret, err
	}
	var serialized []byte
	// But, we convert to OCI format on the wire if it's not already.  The idea here is that by reusing the containers/image
	// stack, clients to this proxy can pretend the world is OCI only, and not need to care about e.g.
	// docker schema and MIME types.
	if manifestType != imgspecv1.MediaTypeImageManifest {
		manifestUpdates := types.ManifestUpdateOptions{ManifestMIMEType: imgspecv1.MediaTypeImageManifest}
		ociImage, err := imgref.img.UpdatedImage(ctx, manifestUpdates)
		if err != nil {
			return ret, err
		}

		ociSerialized, _, err := ociImage.Manifest(ctx)
		if err != nil {
			return ret, err
		}
		serialized = ociSerialized
	} else {
		serialized = rawManifest
	}
	piper, f, err := h.allocPipe()
	if err != nil {
		return ret, err
	}

	go func() {
		// Signal completion when we return
		defer f.wg.Done()
		_, err = io.Copy(f.w, bytes.NewReader(serialized))
		if err != nil {
			f.err = err
		}
	}()

	ret.value = digest.String()
	ret.fd = piper
	ret.pipeid = uint32(f.w.Fd())
	return ret, nil
}

// GetBlob fetches a blob, performing digest verification.
func (h *proxyHandler) GetBlob(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var ret replyBuf

	if h.sysctx == nil {
		return ret, fmt.Errorf("Must invoke Initialize")
	}
	if len(args) != 3 {
		return ret, fmt.Errorf("found %d args, expecting (imgid, digest, size)", len(args))
	}
	imgref, err := h.parseImageFromID(args[0])
	if err != nil {
		return ret, err
	}
	digestStr, ok := args[1].(string)
	if !ok {
		return ret, fmt.Errorf("expecting string blobid")
	}
	size, err := parseUint64(args[2])
	if err != nil {
		return ret, err
	}

	ctx := context.TODO()
	d, err := digest.Parse(digestStr)
	if err != nil {
		return ret, err
	}
	blobr, blobSize, err := imgref.src.GetBlob(ctx, types.BlobInfo{Digest: d, Size: int64(size)}, h.cache)
	if err != nil {
		return ret, err
	}

	piper, f, err := h.allocPipe()
	if err != nil {
		return ret, err
	}
	go func() {
		// Signal completion when we return
		defer f.wg.Done()
		verifier := d.Verifier()
		tr := io.TeeReader(blobr, verifier)
		n, err := io.Copy(f.w, tr)
		if err != nil {
			f.err = err
			return
		}
		if n != int64(size) {
			f.err = fmt.Errorf("Expected %d bytes in blob, got %d", size, n)
		}
		if !verifier.Verified() {
			f.err = fmt.Errorf("corrupted blob, expecting %s", d.String())
		}
	}()

	ret.value = blobSize
	ret.fd = piper
	ret.pipeid = uint32(f.w.Fd())
	return ret, nil
}

// FinishPipe waits for the worker goroutine to finish, and closes the write side of the pipe.
func (h *proxyHandler) FinishPipe(args []interface{}) (replyBuf, error) {
	h.lock.Lock()
	defer h.lock.Unlock()

	var ret replyBuf

	pipeidf, ok := args[0].(float64)
	if !ok {
		return ret, fmt.Errorf("finishpipe: expecting pipeid, not %T", args[0])
	}
	pipeid := uint32(pipeidf)

	f, ok := h.activePipes[pipeid]
	if !ok {
		return ret, fmt.Errorf("finishpipe: no active pipe %d", pipeid)
	}

	// Wait for the goroutine to complete
	f.wg.Wait()
	// And only now do we close the write half; this forces the client to call this API
	f.w.Close()
	// Propagate any errors from the goroutine worker
	err := f.err
	delete(h.activePipes, pipeid)
	return ret, err
}

// send writes a reply buffer to the socket
func (buf replyBuf) send(conn *net.UnixConn, err error) error {
	replyToSerialize := reply{
		Success: err == nil,
		Value:   buf.value,
		PipeID:  buf.pipeid,
	}
	if err != nil {
		replyToSerialize.Error = err.Error()
	}
	serializedReply, err := json.Marshal(&replyToSerialize)
	if err != nil {
		return err
	}
	// We took ownership of the FD - close it when we're done.
	defer func() {
		if buf.fd != nil {
			buf.fd.Close()
		}
	}()
	// Copy the FD number to the socket ancillary buffer
	fds := make([]int, 0)
	if buf.fd != nil {
		fds = append(fds, int(buf.fd.Fd()))
	}
	oob := syscall.UnixRights(fds...)
	n, oobn, err := conn.WriteMsgUnix(serializedReply, oob, nil)
	if err != nil {
		return err
	}
	// Validate that we sent the full packet
	if n != len(serializedReply) || oobn != len(oob) {
		return io.ErrShortWrite
	}
	return nil
}

type proxyOptions struct {
	global    *globalOptions
	imageOpts *imageOptions
	sockFd    int
}

func proxyCmd(global *globalOptions) *cobra.Command {
	sharedFlags, sharedOpts := sharedImageFlags()
	imageFlags, imageOpts := imageFlags(global, sharedOpts, nil, "", "")
	opts := proxyOptions{global: global, imageOpts: imageOpts}
	cmd := &cobra.Command{
		Use:   "experimental-image-proxy [command options] IMAGE",
		Short: "Interactive proxy for fetching container images (EXPERIMENTAL)",
		Long:  `Run skopeo as a proxy, supporting HTTP requests to fetch manifests and blobs.`,
		RunE:  commandAction(opts.run),
		Args:  cobra.ExactArgs(0),
		// Not stabilized yet
		Hidden:  true,
		Example: `skopeo experimental-image-proxy --sockfd 3`,
	}
	adjustUsage(cmd)
	flags := cmd.Flags()
	flags.AddFlagSet(&sharedFlags)
	flags.AddFlagSet(&imageFlags)
	flags.IntVar(&opts.sockFd, "sockfd", 0, "Serve on opened socket pair (default 0/stdin)")
	return cmd
}

// processRequest dispatches a remote request.
// replyBuf is the result of the invocation.
// terminate should be true if processing of requests should halt.
func (h *proxyHandler) processRequest(req request) (rb replyBuf, terminate bool, err error) {
	// Dispatch on the method
	switch req.Method {
	case "Initialize":
		rb, err = h.Initialize(req.Args)
	case "OpenImage":
		rb, err = h.OpenImage(req.Args)
	case "CloseImage":
		rb, err = h.CloseImage(req.Args)
	case "GetManifest":
		rb, err = h.GetManifest(req.Args)
	case "GetBlob":
		rb, err = h.GetBlob(req.Args)
	case "FinishPipe":
		rb, err = h.FinishPipe(req.Args)
	case "Shutdown":
		terminate = true
	default:
		err = fmt.Errorf("unknown method: %s", req.Method)
	}
	return
}

// Implementation of podman experimental-image-proxy
func (opts *proxyOptions) run(args []string, stdout io.Writer) error {
	handler := &proxyHandler{
		opts:        opts,
		images:      make(map[uint32]*openImage),
		activePipes: make(map[uint32]*activePipe),
	}

	// Convert the socket FD passed by client into a net.FileConn
	fd := os.NewFile(uintptr(opts.sockFd), "sock")
	fconn, err := net.FileConn(fd)
	if err != nil {
		return err
	}
	conn := fconn.(*net.UnixConn)

	// Allocate a buffer to copy the packet into
	buf := make([]byte, maxMsgSize)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("reading socket: %v", err)
		}
		// Parse the request JSON
		readbuf := buf[0:n]
		var req request
		if err := json.Unmarshal(readbuf, &req); err != nil {
			rb := replyBuf{}
			rb.send(conn, fmt.Errorf("invalid request: %v", err))
		}

		rb, terminate, err := handler.processRequest(req)
		if terminate {
			return nil
		}
		rb.send(conn, err)
	}
}
