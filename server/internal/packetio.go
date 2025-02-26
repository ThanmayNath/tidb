// Copyright 2015 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Copyright 2013 The Go-MySQL-Driver Authors. All rights reserved.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this file,
// You can obtain one at http://mozilla.org/MPL/2.0/.

// The MIT License (MIT)
//
// Copyright (c) 2014 wandoulabs
// Copyright (c) 2014 siddontang
//
// Permission is hereby granted, free of charge, to any person obtaining a copy of
// this software and associated documentation files (the "Software"), to deal in
// the Software without restriction, including without limitation the rights to
// use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies of
// the Software, and to permit persons to whom the Software is furnished to do so,
// subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.

package internal

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"io"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/pingcap/errors"
	"github.com/pingcap/tidb/parser/mysql"
	"github.com/pingcap/tidb/parser/terror"
	"github.com/pingcap/tidb/server/err"
	"github.com/pingcap/tidb/server/internal/util"
	server_metrics "github.com/pingcap/tidb/server/metrics"
	"github.com/pingcap/tidb/sessionctx/variable"
)

const defaultWriterSize = 16 * 1024

// PacketIO is a helper to read and write data in packet format.
// MySQL Packets: https://dev.mysql.com/doc/internals/en/mysql-packet.html
type PacketIO struct {
	bufReadConn      *util.BufferedReadConn
	bufWriter        *bufio.Writer
	compressedWriter *compressedWriter
	readTimeout      time.Duration
	// maxAllowedPacket is the maximum size of one packet in ReadPacket.
	maxAllowedPacket uint64
	// accumulatedLength count the length of totally received 'payload' in ReadPacket.
	accumulatedLength    uint64
	compressionAlgorithm int
	zstdLevel            zstd.EncoderLevel
	sequence             uint8
	compressedSequence   uint8
}

// NewPacketIO creates a new PacketIO with given net.Conn.
func NewPacketIO(bufReadConn *util.BufferedReadConn) *PacketIO {
	p := &PacketIO{sequence: 0, compressionAlgorithm: mysql.CompressionNone, compressedSequence: 0, zstdLevel: 3}
	p.SetBufferedReadConn(bufReadConn)
	p.SetMaxAllowedPacket(variable.DefMaxAllowedPacket)
	return p
}

// NewPacketIOForTest creates a new PacketIO with given bufio.Writer.
func NewPacketIOForTest(bufWriter *bufio.Writer) *PacketIO {
	p := &PacketIO{}
	p.SetBufWriter(bufWriter)
	return p
}

// SetZstdLevel sets the zstd compression level.
func (p *PacketIO) SetZstdLevel(level zstd.EncoderLevel) {
	p.zstdLevel = level
}

// Sequence returns the sequence of PacketIO.
func (p *PacketIO) Sequence() uint8 {
	return p.sequence
}

// SetSequence sets the sequence of PacketIO.
func (p *PacketIO) SetSequence(s uint8) {
	p.sequence = s
}

// SetCompressedSequence sets the compressed sequence of PacketIO.
func (p *PacketIO) SetCompressedSequence(s uint8) {
	p.compressedSequence = s
}

// SetBufWriter sets the bufio.Writer of PacketIO.
func (p *PacketIO) SetBufWriter(bufWriter *bufio.Writer) {
	p.bufWriter = bufWriter
}

// ResetBufWriter resets the bufio.Writer of PacketIO.
func (p *PacketIO) ResetBufWriter(w io.Writer) {
	p.bufWriter.Reset(w)
}

// SetCompressionAlgorithm sets the compression algorithm of PacketIO.
func (p *PacketIO) SetCompressionAlgorithm(ca int) {
	p.compressionAlgorithm = ca
	p.compressedWriter = newCompressedWriter(p.bufReadConn, ca)
	p.compressedWriter.zstdLevel = p.zstdLevel
	p.bufWriter.Flush()
}

// SetBufferedReadConn sets the BufferedReadConn of PacketIO.
func (p *PacketIO) SetBufferedReadConn(bufReadConn *util.BufferedReadConn) {
	p.bufReadConn = bufReadConn
	p.bufWriter = bufio.NewWriterSize(bufReadConn, defaultWriterSize)
}

// SetReadTimeout sets the read timeout of PacketIO.
func (p *PacketIO) SetReadTimeout(timeout time.Duration) {
	p.readTimeout = timeout
}

func (p *PacketIO) readOnePacket() ([]byte, error) {
	var header [4]byte
	r := io.NopCloser(p.bufReadConn)
	if p.readTimeout > 0 {
		if err := p.bufReadConn.SetReadDeadline(time.Now().Add(p.readTimeout)); err != nil {
			return nil, err
		}
	}
	if p.compressionAlgorithm != mysql.CompressionNone {
		var compressedHeader [7]byte
		if _, err := io.ReadFull(p.bufReadConn, compressedHeader[:]); err != nil {
			return nil, errors.Trace(err)
		}
		compressedSequence := compressedHeader[3]
		if compressedSequence != p.compressedSequence {
			return nil, err.ErrInvalidSequence.GenWithStack(
				"invalid compressed sequence %d != %d", compressedSequence, p.compressedSequence)
		}
		p.compressedSequence++
		p.compressedWriter.compressedSequence = p.compressedSequence
		uncompressedLength := int(uint32(compressedHeader[4]) | uint32(compressedHeader[5])<<8 | uint32(compressedHeader[6])<<16)

		if uncompressedLength > 0 {
			switch p.compressionAlgorithm {
			case mysql.CompressionZlib:
				var err error
				r, err = zlib.NewReader(p.bufReadConn)
				if err != nil {
					return nil, errors.Trace(err)
				}
			case mysql.CompressionZstd:
				zstdReader, err := zstd.NewReader(p.bufReadConn, zstd.WithDecoderConcurrency(1))
				if err != nil {
					return nil, errors.Trace(err)
				}
				r = zstdReader.IOReadCloser()
			default:
				return nil, errors.New("Unknown compression algorithm")
			}
		}
	}
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, errors.Trace(err)
	}

	sequence := header[3]
	if sequence != p.sequence {
		return nil, err.ErrInvalidSequence.GenWithStack("invalid sequence %d != %d", sequence, p.sequence)
	}

	p.sequence++

	length := int(uint32(header[0]) | uint32(header[1])<<8 | uint32(header[2])<<16)

	// Accumulated payload length exceeds the limit.
	if p.accumulatedLength += uint64(length); p.accumulatedLength > p.maxAllowedPacket {
		terror.Log(err.ErrNetPacketTooLarge)
		return nil, err.ErrNetPacketTooLarge
	}

	data := make([]byte, length)
	if p.readTimeout > 0 {
		if err := p.bufReadConn.SetReadDeadline(time.Now().Add(p.readTimeout)); err != nil {
			return nil, err
		}
	}
	if _, err := io.ReadFull(r, data); err != nil {
		return nil, errors.Trace(err)
	}
	err := r.Close()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return data, nil
}

// SetMaxAllowedPacket sets the max allowed packet size of PacketIO.
func (p *PacketIO) SetMaxAllowedPacket(maxAllowedPacket uint64) {
	p.maxAllowedPacket = maxAllowedPacket
}

// ReadPacket reads a packet from the connection.
func (p *PacketIO) ReadPacket() ([]byte, error) {
	p.accumulatedLength = 0
	if p.readTimeout == 0 {
		if err := p.bufReadConn.SetReadDeadline(time.Time{}); err != nil {
			return nil, errors.Trace(err)
		}
	}
	data, err := p.readOnePacket()
	if err != nil {
		return nil, errors.Trace(err)
	}

	if len(data) < mysql.MaxPayloadLen {
		server_metrics.ReadPacketBytes.Add(float64(len(data)))
		return data, nil
	}

	// handle multi-packet
	for {
		buf, err := p.readOnePacket()
		if err != nil {
			return nil, errors.Trace(err)
		}

		data = append(data, buf...)

		if len(buf) < mysql.MaxPayloadLen {
			break
		}
	}

	server_metrics.ReadPacketBytes.Add(float64(len(data)))
	return data, nil
}

// WritePacket writes data that already have header
func (p *PacketIO) WritePacket(data []byte) error {
	length := len(data) - 4
	server_metrics.WritePacketBytes.Add(float64(len(data)))

	maxPayloadLen := mysql.MaxPayloadLen
	if p.compressionAlgorithm != mysql.CompressionNone {
		maxPayloadLen -= 4
	}

	for length >= maxPayloadLen {
		data[3] = p.sequence
		data[0] = 0xff
		data[1] = 0xff
		data[2] = 0xff

		if p.compressionAlgorithm != mysql.CompressionNone {
			if n, err := p.compressedWriter.Write(data[:4+maxPayloadLen]); err != nil {
				return errors.Trace(mysql.ErrBadConn)
			} else if n != (4 + maxPayloadLen) {
				return errors.Trace(mysql.ErrBadConn)
			}
		} else {
			if n, err := p.bufWriter.Write(data[:4+maxPayloadLen]); err != nil {
				return errors.Trace(mysql.ErrBadConn)
			} else if n != (4 + maxPayloadLen) {
				return errors.Trace(mysql.ErrBadConn)
			}
		}
		p.sequence++
		length -= maxPayloadLen
		data = data[maxPayloadLen:]
	}
	data[3] = p.sequence
	data[0] = byte(length)
	data[1] = byte(length >> 8)
	data[2] = byte(length >> 16)

	if p.compressionAlgorithm != mysql.CompressionNone {
		if n, err := p.compressedWriter.Write(data); err != nil {
			terror.Log(errors.Trace(err))
			return errors.Trace(mysql.ErrBadConn)
		} else if n != len(data) {
			return errors.Trace(mysql.ErrBadConn)
		} else {
			p.sequence++
			return nil
		}
	} else {
		if n, err := p.bufWriter.Write(data); err != nil {
			terror.Log(errors.Trace(err))
			return errors.Trace(mysql.ErrBadConn)
		} else if n != len(data) {
			return errors.Trace(mysql.ErrBadConn)
		} else {
			p.sequence++
			return nil
		}
	}
}

// Flush flushes buffered data to network.
func (p *PacketIO) Flush() error {
	var err error
	if p.compressionAlgorithm != mysql.CompressionNone {
		err = p.compressedWriter.Flush()
	} else {
		err = p.bufWriter.Flush()
	}
	if err != nil {
		return errors.Trace(err)
	}
	return err
}

func newCompressedWriter(w io.Writer, ca int) *compressedWriter {
	return &compressedWriter{
		w,
		new(bytes.Buffer),
		ca,
		0,
		3,
	}
}

type compressedWriter struct {
	w                    io.Writer
	buf                  *bytes.Buffer
	compressionAlgorithm int
	compressedSequence   uint8
	zstdLevel            zstd.EncoderLevel
}

func (cw *compressedWriter) Write(data []byte) (n int, err error) {
	// MySQL starts with `net_buffer_length` (default 16384) and larger packets after that.
	// The length itself must fit in the 3 byte field in the header.
	// Can't be bigger then the max value for `net_buffer_length` (1048576)
	maxCompressedSize := 1048576 // 1 MiB

	for {
		remainingLen := maxCompressedSize - cw.buf.Len()
		if len(data) <= remainingLen {
			written, err := cw.buf.Write(data)
			if err != nil {
				return 0, err
			}
			return n + written, nil
		}
		written, err := cw.buf.Write(data[:remainingLen])
		if err != nil {
			return 0, err
		}
		n += written
		data = data[remainingLen:]
		err = cw.Flush()
		if err != nil {
			return 0, err
		}
	}
}

func (cw *compressedWriter) Flush() error {
	var payload, compressedPacket bytes.Buffer
	var w io.WriteCloser
	var err error

	// https://dev.mysql.com/doc/dev/mysql-server/latest/page_protocol_basic_compression_packet.html
	// suggests a MIN_COMPRESS_LENGTH of 50.
	minCompressLength := 50
	zlibCompressDefaultLevel := 6
	data := cw.buf.Bytes()
	cw.buf.Reset()

	switch cw.compressionAlgorithm {
	case mysql.CompressionZlib:
		w, err = zlib.NewWriterLevel(&payload, zlibCompressDefaultLevel)
	case mysql.CompressionZstd:
		w, err = zstd.NewWriter(&payload, zstd.WithEncoderLevel(cw.zstdLevel))
	default:
		return errors.New("Unknown compression algorithm")
	}
	if err != nil {
		return errors.Trace(err)
	}

	uncompressedLength := 0
	compressedHeader := make([]byte, 7)

	if len(data) > minCompressLength {
		uncompressedLength = len(data)
		_, err := w.Write(data)
		if err != nil {
			return errors.Trace(err)
		}
		err = w.Close()
		if err != nil {
			return errors.Trace(err)
		}
	}

	var compressedLength int
	if len(data) > minCompressLength {
		compressedLength = len(payload.Bytes())
	} else {
		compressedLength = len(data)
	}
	compressedHeader[0] = byte(compressedLength)
	compressedHeader[1] = byte(compressedLength >> 8)
	compressedHeader[2] = byte(compressedLength >> 16)
	compressedHeader[3] = cw.compressedSequence
	compressedHeader[4] = byte(uncompressedLength)
	compressedHeader[5] = byte(uncompressedLength >> 8)
	compressedHeader[6] = byte(uncompressedLength >> 16)
	_, err = compressedPacket.Write(compressedHeader)
	if err != nil {
		return errors.Trace(err)
	}
	cw.compressedSequence++

	if len(data) > minCompressLength {
		_, err = compressedPacket.Write(payload.Bytes())
	} else {
		_, err = compressedPacket.Write(data)
	}
	if err != nil {
		return errors.Trace(err)
	}
	w.Close()
	_, err = cw.w.Write(compressedPacket.Bytes())
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}
