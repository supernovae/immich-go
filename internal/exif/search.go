package exif

import (
	"bufio"
	"bytes"
	"io"
)

type sliceReader struct {
	bufio.Reader
}

func newSliceReader(r io.Reader) *sliceReader {
	return &sliceReader{
		Reader: *bufio.NewReader(r),
	}
}

func (r *sliceReader) ReadSlice(l int) ([]byte, error) {
	b := make([]byte, l)
	_, err := r.Read(b)
	return b, err
}

func searchPattern(r io.Reader, pattern []byte, buffer []byte) (*sliceReader, error) {
	var err error
	pos := 0
	ofs := 0

	var bytesRead int
	for {
		// Read a chunk of data into the buffer
		bytesRead, err = r.Read(buffer[ofs:])
		if bytesRead > 0 {
			total := ofs + bytesRead
			// Search for the pattern within the buffer
			index := bytes.Index(buffer[:total], pattern)
			if index >= 0 {
				return newSliceReader(io.MultiReader(bytes.NewReader(buffer[index:total]), r)), nil
			}

			// Move the remaining bytes of the current buffer to the beginning
			keep := min(len(pattern)-1, total)
			if keep > 0 {
				copy(buffer, buffer[total-keep:total])
			}
			ofs = keep
			pos += bytesRead
		}
		if err != nil {
			return nil, err
		}
		if bytesRead == 0 {
			return nil, io.EOF
		}
	}
}
