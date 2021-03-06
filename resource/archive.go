/*
Copyright (C) 2016-2018 Andreas T Jonsson

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU General Public License as published by
the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU General Public License for more details.

You should have received a copy of the GNU General Public License
along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

/*
The header is pretty short and consists of an Archive ID and the number of file entries
that are to be found in the File Table. PreRelease archives to not feature the Archive ID.

The File Table holds offsets to all the files that are stored inside the Data section. Files
are not named, so their index in the File Table has to be fixed, which means that stripped
down versions of the game have placeholders. Placeholders in the PreRelease demos and
DOS Shareware are FF FF FF FF and under Mac they are 00 00 00 00. In the retail version
they are marked by a follwing offset just 1 greater.

Each non-placeholder data entry begins with its unpacked size as a 4 byte integer. If
the third highest bit (20 00 00 00) is set, the file is compressed, else it’s just raw data.
filesize & 0x1FFFFFFF returns the correct size of the file. The length of the data
is calculated as offsets[n+1]-offsets[n]-4, using the size of the .WAR file as final
offset, as usual.
*/

package resource

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
)

var (
	dosRetail    = [...]byte{0x18, 0x0, 0x0, 0x0}
	dosShareware = [...]byte{0x19, 0x0, 0x0, 0x0}
	macRetail    = [...]byte{0x0, 0x0, 0x0, 0x1A}
	macShareware = [...]byte{0x0, 0x0, 0x0, 0x19}
)

var (
	ErrUnsupportedVersion = errors.New("unsupported version")
	Logger                = log.New(ioutil.Discard, "", 0)
	LoadUnsupported       = false
)

type Archive struct {
	Type  string
	Files map[string][]byte
}

func (a *Archive) Open(file string) (io.Reader, error) {
	if f, ok := a.Files[file]; ok {
		return bytes.NewReader(f), nil
	}
	return nil, os.ErrNotExist
}

func OpenArchive(file string) (*Archive, error) {
	fp, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer fp.Close()

	info, err := fp.Stat()
	if err != nil {
		return nil, err
	}

	return OpenArchiveFrom(fp, info.Size())
}

func OpenArchiveFrom(fp io.ReadSeeker, sz int64) (*Archive, error) {
	var (
		err       error
		archiveID [4]byte
	)

	if _, err = fp.Read(archiveID[:]); err != nil {
		return nil, err
	}

	arch := &Archive{"", make(map[string][]byte)}

	Logger.Print("Archive ID: ")
	switch archiveID {
	case dosRetail:
		arch.Type = "DOS Retail"
	case dosShareware:
		arch.Type = "DOS Shareware"
	default:
		switch archiveID {
		case macRetail:
			arch.Type = "Mac Retail"
		case macShareware:
			arch.Type = "Mac Shareware"
		default:
			return nil, errors.New("unknown version")
		}
		return nil, ErrUnsupportedVersion
	}
	Logger.Println(arch.Type)

	var numFiles uint32
	if err = binary.Read(fp, binary.LittleEndian, &numFiles); err != nil {
		return nil, err
	}
	Logger.Println("Number of files in archive: ", numFiles)

	if int(numFiles) != len(fileMap) {
		return nil, errors.New("table mapping mismatch")
	}

	fileTable := make([]uint32, numFiles)
	for i := range fileTable {
		if err = binary.Read(fp, binary.LittleEndian, &fileTable[i]); err != nil {
			return nil, err
		}
	}

	for i, offset := range fileTable {
		if isPlaceHolder(fileTable, offset, i) {
			if fileMap[i] != "" {
				Logger.Printf("Incomplete WAR file. Missing '%v'.\n", fileMap[i])
			}

			Logger.Println("Skipping placeholder: ", i)
			continue
		}

		if _, err = fp.Seek(int64(offset), 0); err != nil {
			return nil, err
		}

		var size uint32
		if err = binary.Read(fp, binary.LittleEndian, &size); err != nil {
			return nil, err
		}

		isCompressed := size>>24 == 0x20
		size &= 0x00FFFFFF

		var dataLength uint32
		if i == len(fileTable)-1 {
			dataLength = uint32(sz) - fileTable[i]
		} else {
			dataLength = fileTable[i+1] - fileTable[i]
		}
		dataLength -= 4

		fileName := fileMap[i]
		if fileName == "" {
			if !LoadUnsupported {
				continue
			}

			Logger.Printf("Warning: Filename table is incomplete! Missing file with id %v.\n", i)
			fileName = fmt.Sprintf("DATA.WAR.%v", i)
		}

		var data []byte
		if isCompressed {
			Logger.Printf("Compressed entry: #%v %s\n", i, fileName)
			if data, err = uncompressData(fp, int(size), int(dataLength)); err != nil {
				return nil, err
			}
		} else {
			Logger.Printf("Uncompressed entry: #%v %s\n", i, fileName)
			data = make([]byte, size)
			if num, err := fp.Read(data); num != len(data) || err != nil {
				return nil, err
			}
		}

		arch.Files[fileName] = data
	}

	return arch, nil
}

func isPlaceHolder(tab []uint32, offset uint32, i int) bool {
	if offset == 0x0 || offset == 0xFFFFFFFF {
		return true
	}

	// Perhaps we should use the archive size?
	if i == len(tab)-1 {
		return false
	}

	if offset == (tab[i+1] - 1) {
		return true
	}
	return false
}

func readByte(reader io.Reader) (byte, error) {
	var b [1]byte
	if n, err := reader.Read(b[:]); n != 1 || err != nil {
		return 0, err
	}
	return b[0], nil
}

func readShort(reader io.Reader) (uint16, error) {
	var short uint16
	if err := binary.Read(reader, binary.LittleEndian, &short); err != nil {
		return 0, err
	}
	return short, nil
}

/*
The DOS version archives of WarCraft are compressed using a sort of LZ compression.
This means that at compression time, the algorithm checked if there was the exact same
sequence of bytes previously written, as is being written now.
*/

func uncompressData(reader io.Reader, fileSize, dataSize int) ([]byte, error) {
	const bufferSize = 4096
	var backingBuffer bytes.Buffer

	writer := bufio.NewWriter(&backingBuffer)
	buffer := make([]byte, bufferSize)

	var (
		numWrite,
		numRead int
	)

	for numRead < dataSize {
		cmask, err := readByte(reader)
		numRead++

		if err != nil {
			return buffer, err
		}

		for i := 0; i < 8 && numWrite != fileSize; i++ {
			if cmask%2 == 1 { // uncompressed
				bufByte, err := readByte(reader)
				numRead++

				if err != nil {
					return buffer, err
				}

				buffer[numWrite%bufferSize] = bufByte
				writer.WriteByte(bufByte)
				numWrite++
			} else { // compressed
				offset, err := readShort(reader)
				numRead += 2

				if err != nil {
					return buffer, err
				}

				numBytes := offset / bufferSize
				offset %= bufferSize

				for m := uint16(0); m <= numBytes+2; m++ {
					bufByte := buffer[(offset+m)%bufferSize]
					buffer[numWrite%bufferSize] = bufByte

					writer.WriteByte(bufByte)
					numWrite++
				}
			}
			cmask /= 2
		}
	}

	writer.Flush()
	return backingBuffer.Bytes(), nil
}
