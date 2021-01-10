// Copyright 2021 The gg Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//		 https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"gg-scm.io/pkg/git/githash"
	"gg-scm.io/pkg/git/internal/pktline"
)

// Reference:
// https://git-scm.com/docs/pack-protocol
// https://git-scm.com/docs/http-protocol (because we're using stateless)

const v1ExtraParams = "version=1"

type fetchV1 struct {
	caps       capabilityList
	impl       impl
	refsReader *pktline.Reader
	refsCloser io.Closer

	refs      []*Ref
	refsError error
}

func newFetchV1(impl impl, refsReader *pktline.Reader, refsCloser io.Closer) *fetchV1 {
	f := &fetchV1{impl: impl}
	var ref0 *Ref
	ref0, f.caps, f.refsError = readFirstRefV1(refsReader)
	if ref0 == nil {
		// Either an error or only capabilities were received.
		// No need to hang onto refsReader.
		refsCloser.Close()
		return f
	}
	f.refs = []*Ref{ref0}
	f.refsReader = refsReader
	f.refsCloser = refsCloser
	return f
}

func (f *fetchV1) Close() error {
	if f.refsCloser != nil {
		return f.refsCloser.Close()
	}
	return nil
}

func (f *fetchV1) listRefs(ctx context.Context, refPrefixes []string) ([]*Ref, error) {
	if f.refsReader != nil {
		f.refs, f.refsError = readOtherRefsV1(f.refs, f.caps.symrefs(), f.refsReader)
		f.refsCloser.Close()
		f.refsReader = nil
		f.refsCloser = nil
	}
	if len(refPrefixes) == 0 {
		return append([]*Ref(nil), f.refs...), f.refsError
	}
	// Filter by given prefixes.
	refs := make([]*Ref, 0, len(f.refs))
	for _, r := range f.refs {
		for _, prefix := range refPrefixes {
			if strings.HasPrefix(string(r.Name), prefix) {
				refs = append(refs, r)
			}
		}
	}
	return refs, f.refsError
}

// readFirstRefV1 reads the first ref in the version 1 refs advertisement
// response, skipping the "version 1" line if necessary. The caller is expected
// to have advanced r to the first line before calling readFirstRefV1.
func readFirstRefV1(r *pktline.Reader) (*Ref, capabilityList, error) {
	line, err := r.Text()
	if err != nil {
		return nil, nil, fmt.Errorf("read refs: first ref: %w", err)
	}
	if bytes.Equal(line, []byte("version 1")) {
		// Skip optional initial "version 1" packet.
		r.Next()
		line, err = r.Text()
		if err != nil {
			return nil, nil, fmt.Errorf("read refs: first ref: %w", err)
		}
	}
	ref0, caps, err := parseFirstRefV1(line)
	if err != nil {
		return nil, nil, fmt.Errorf("read refs: %w", err)
	}
	if ref0 == nil {
		// Expect flush next.
		// TODO(someday): Or shallow?
		if !r.Next() {
			return nil, nil, fmt.Errorf("read refs: %w", r.Err())
		}
		if r.Type() != pktline.Flush {
			return nil, nil, fmt.Errorf("read refs: expected flush after no-refs")
		}
		return nil, caps, nil
	}
	return ref0, caps, nil
}

func parseFirstRefV1(line []byte) (*Ref, capabilityList, error) {
	refEnd := bytes.IndexByte(line, 0)
	if refEnd == -1 {
		return nil, nil, fmt.Errorf("first ref: missing nul")
	}
	idEnd := bytes.IndexByte(line[:refEnd], ' ')
	if idEnd == -1 {
		return nil, nil, fmt.Errorf("first ref: missing space")
	}
	id, err := githash.ParseSHA1(string(line[:idEnd]))
	if err != nil {
		return nil, nil, fmt.Errorf("first ref: %w", err)
	}
	refName := githash.Ref(line[idEnd+1 : refEnd])
	caps := make(capabilityList)
	for _, c := range bytes.Fields(line[refEnd+1:]) {
		k, v, err := parseCapability(c)
		if err != nil {
			return nil, nil, fmt.Errorf("first ref: %w", err)
		}
		if k == symrefCap {
			caps.addSymref(v)
		} else {
			caps[k] = v
		}
	}
	if refName == "capabilities^{}" {
		if id != (githash.SHA1{}) {
			return nil, nil, fmt.Errorf("first ref: non-zero ID passed with no-refs response")
		}
		return nil, caps, nil
	}
	if !refName.IsValid() {
		return nil, nil, fmt.Errorf("first ref %q: invalid name", refName)
	}
	return &Ref{
		ObjectID:     id,
		Name:         refName,
		SymrefTarget: caps.symrefs()[refName],
	}, caps, nil
}

// readOtherRefsV1 parses the second and subsequent refs in the version 1 refs
// advertisement response. The caller is expected to have advanced r past the
// first ref before calling readOtherRefsV1.
func readOtherRefsV1(refs []*Ref, symrefs map[githash.Ref]githash.Ref, r *pktline.Reader) ([]*Ref, error) {
	for r.Next() && r.Type() != pktline.Flush {
		line, err := r.Text()
		if err != nil {
			return nil, fmt.Errorf("read refs: %w", err)
		}
		ref, err := parseOtherRefV1(line)
		if err != nil {
			return nil, fmt.Errorf("read refs: %w", err)
		}
		ref.SymrefTarget = symrefs[ref.Name]
		refs = append(refs, ref)
	}
	if err := r.Err(); err != nil {
		return refs, fmt.Errorf("read refs: %w", err)
	}
	return refs, nil
}

func parseOtherRefV1(line []byte) (*Ref, error) {
	line = trimLF(line)
	idEnd := bytes.IndexByte(line, ' ')
	if idEnd == -1 {
		return nil, fmt.Errorf("ref: missing space")
	}
	refName := githash.Ref(line[idEnd+1:])
	if !refName.IsValid() {
		return nil, fmt.Errorf("ref %q: invalid name", refName)
	}
	id, err := githash.ParseSHA1(string(line[:idEnd]))
	if err != nil {
		return nil, fmt.Errorf("ref %s: %w", refName, err)
	}
	return &Ref{
		ObjectID: id,
		Name:     refName,
	}, nil
}

func (f *fetchV1) negotiate(ctx context.Context, errPrefix string, req *FetchRequest) (*FetchResponse, error) {
	// Determine which capabilities we can use.
	useCaps := capabilityList{
		multiAckCap: "",
		ofsDeltaCap: "",
	}
	if req.Progress == nil {
		useCaps[noProgressCap] = ""
	}
	useCaps.intersect(f.caps)
	// From https://git-scm.com/docs/protocol-capabilities, "[t]he client MUST
	// send only maximum [sic] of one of 'side-band' and [sic] 'side-band-64k'."
	switch {
	case f.caps.supports(sideBand64KCap):
		useCaps[sideBand64KCap] = ""
	case f.caps.supports(sideBandCap):
		useCaps[sideBandCap] = ""
	default:
		// TODO(someday): Support reading without demuxing.
		return nil, fmt.Errorf("remote does not support side-band")
	}

	var commandBuf []byte
	commandBuf = pktline.AppendString(commandBuf, fmt.Sprintf("want %v %v\n", req.Want[0], useCaps))
	for _, want := range req.Want[1:] {
		commandBuf = pktline.AppendString(commandBuf, "want "+want.String()+"\n")
	}
	commandBuf = pktline.AppendFlush(commandBuf)
	for _, have := range req.Have {
		commandBuf = pktline.AppendString(commandBuf, "have "+have.String()+"\n")
	}
	if !req.HaveMore {
		commandBuf = pktline.AppendString(commandBuf, "done")
	} else {
		commandBuf = pktline.AppendFlush(commandBuf)
	}
	resp, err := f.impl.uploadPack(ctx, v1ExtraParams, bytes.NewReader(commandBuf))
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			resp.Close()
		}
	}()

	respReader := pktline.NewReader(resp)
	result := &FetchResponse{
		Acks: make(map[githash.SHA1]bool),
	}
	foundCommonBase := false
ackLoop:
	for respReader.Next() {
		line, err := respReader.Text()
		if err != nil {
			return nil, fmt.Errorf("parse response: %w", err)
		}
		switch {
		case bytes.HasPrefix(line, []byte(ackPrefix)):
			line = line[len(ackPrefix):]
			var id githash.SHA1
			idEnd := hex.EncodedLen(len(id))
			if idEnd > len(line) {
				return nil, fmt.Errorf("parse response: acknowledgements: ack too short")
			}
			if err := id.UnmarshalText(line[:idEnd]); err != nil {
				return nil, fmt.Errorf("parse response: acknowledgements: %w", err)
			}
			result.Acks[id] = true
			switch status := line[idEnd:]; {
			case len(status) == 0:
				foundCommonBase = true
				break ackLoop
			case bytes.Equal(status, []byte("continue")):
				// Only valid status for multi_ack
			default:
				return nil, fmt.Errorf("parse response: acknowledgements: unknown status %q", status)
			}
		case bytes.Equal(line, []byte(nak)):
			break ackLoop
		default:
			return nil, fmt.Errorf("parse response: acknowledgements: unrecognized directive %q", line)
		}
	}
	if err := respReader.Err(); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if foundCommonBase || !req.HaveMore {
		result.Packfile = &packfileReader{
			errPrefix:  errPrefix,
			packReader: respReader,
			packCloser: resp,
			progress:   req.Progress,
		}
	}
	return result, nil
}
