//go:build !integration
// +build !integration

package iso9660

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// buildTestISO constructs a minimal ISO9660 image in memory with the given
// root directory entries written at the root directory extent.
// It returns the raw image bytes.
func buildTestISO(t *testing.T, rootDirEntries []DirectoryEntry, fileData map[int32][]byte) []byte {
	t.Helper()

	// Layout:
	//   sectors 0-15: system area (zeroes)
	//   sector 16:    primary volume descriptor
	//   sector 17:    terminator volume descriptor
	//   sector 18:    root directory extent (1 sector)
	//   sector 19+:   file data

	totalSectors := 20
	// Ensure we have enough sectors for all file data
	for loc, data := range fileData {
		needed := int(loc) + (len(data)+int(sectorSize)-1)/int(sectorSize) + 1
		if needed > totalSectors {
			totalSectors = needed
		}
	}

	img := make([]byte, totalSectors*int(sectorSize))

	// Build primary volume descriptor at sector 16
	pvdSector := img[16*sectorSize : 17*sectorSize]
	pvdSector[0] = volumeTypePrimary
	copy(pvdSector[1:6], "CD001")
	pvdSector[6] = 1

	// Volume identifier at offset 40
	copy(pvdSector[40:72], MarshalString("TESTISO", 32))

	// Volume space size at offset 80 (both-endian)
	WriteInt32LSBMSB(pvdSector[80:88], int32(totalSectors))

	// Volume set size at offset 120
	WriteInt16LSBMSB(pvdSector[120:124], 1)
	// Volume sequence number at offset 124
	WriteInt16LSBMSB(pvdSector[124:128], 1)
	// Logical block size at offset 128
	WriteInt16LSBMSB(pvdSector[128:132], int16(sectorSize))

	// Root directory entry at offset 156
	rootDE := DirectoryEntry{
		ExtentLocation:       18,
		ExtentLength:         sectorSize, // 1 sector
		RecordingDateTime:    RecordingTimestamp{},
		FileFlags:            dirFlagDir,
		VolumeSequenceNumber: 1,
		Identifier:           string([]byte{0}),
		SystemUse:            []byte{},
	}
	rdeBytes, err := rootDE.MarshalBinary()
	require.NoError(t, err)
	copy(pvdSector[156:190], rdeBytes)

	// File structure version
	pvdSector[881] = 1

	// Build terminator volume descriptor at sector 17
	termSector := img[17*sectorSize : 18*sectorSize]
	termSector[0] = volumeTypeTerminator
	copy(termSector[1:6], "CD001")
	termSector[6] = 1

	// Build root directory at sector 18
	dirSector := img[18*sectorSize : 19*sectorSize]
	offset := 0

	for _, de := range rootDirEntries {
		data, err := de.MarshalBinary()
		require.NoError(t, err)
		copy(dirSector[offset:], data)
		offset += len(data)
	}

	// Write file data
	for loc, data := range fileData {
		fileOffset := int(loc) * int(sectorSize)
		copy(img[fileOffset:], data)
	}

	return img
}

func TestMultiExtentFile(t *testing.T) {
	// Create a file split across 3 extents:
	// Extent 1: sector 19, 100 bytes  (multi-extent flag set)
	// Extent 2: sector 20, 100 bytes  (multi-extent flag set)
	// Extent 3: sector 21, 50 bytes   (final, no multi-extent flag)

	extent1Data := bytes.Repeat([]byte("A"), 100)
	extent2Data := bytes.Repeat([]byte("B"), 100)
	extent3Data := bytes.Repeat([]byte("C"), 50)

	// "." entry
	dotEntry := DirectoryEntry{
		ExtentLocation:       18,
		ExtentLength:         sectorSize,
		FileFlags:            dirFlagDir,
		VolumeSequenceNumber: 1,
		Identifier:           string([]byte{0}),
		SystemUse:            []byte{},
	}

	// ".." entry
	dotDotEntry := DirectoryEntry{
		ExtentLocation:       18,
		ExtentLength:         sectorSize,
		FileFlags:            dirFlagDir,
		VolumeSequenceNumber: 1,
		Identifier:           string([]byte{1}),
		SystemUse:            []byte{},
	}

	// Multi-extent records for "BIGFILE.BIN;1"
	me1 := DirectoryEntry{
		ExtentLocation:       19,
		ExtentLength:         100,
		FileFlags:            dirFlagMultiExtent, // not final
		VolumeSequenceNumber: 1,
		Identifier:           "BIGFILE.BIN;1",
		SystemUse:            []byte{},
	}

	me2 := DirectoryEntry{
		ExtentLocation:       20,
		ExtentLength:         100,
		FileFlags:            dirFlagMultiExtent, // not final
		VolumeSequenceNumber: 1,
		Identifier:           "BIGFILE.BIN;1",
		SystemUse:            []byte{},
	}

	me3 := DirectoryEntry{
		ExtentLocation:       21,
		ExtentLength:         50,
		FileFlags:            0, // final record
		VolumeSequenceNumber: 1,
		Identifier:           "BIGFILE.BIN;1",
		SystemUse:            []byte{},
	}

	entries := []DirectoryEntry{dotEntry, dotDotEntry, me1, me2, me3}
	fileData := map[int32][]byte{
		19: extent1Data,
		20: extent2Data,
		21: extent3Data,
	}

	img := buildTestISO(t, entries, fileData)

	image, err := OpenImage(bytes.NewReader(img))
	require.NoError(t, err)

	rootDir, err := image.RootDir()
	require.NoError(t, err)

	children, err := rootDir.GetChildren()
	require.NoError(t, err)

	// Should have exactly 1 file (the 3 multi-extent records merged into 1)
	require.Len(t, children, 1)

	bigfile := children[0]
	assert.Equal(t, "BIGFILE.BIN", bigfile.Name())
	assert.Equal(t, int64(250), bigfile.Size()) // 100 + 100 + 50
	assert.False(t, bigfile.IsDir())
	assert.Len(t, bigfile.extents, 3)

	// Read all data and verify it's the concatenation of all extents
	data, err := io.ReadAll(bigfile.Reader())
	require.NoError(t, err)
	assert.Len(t, data, 250)

	expected := make([]byte, 0, 250)
	expected = append(expected, extent1Data...)
	expected = append(expected, extent2Data...)
	expected = append(expected, extent3Data...)
	assert.Equal(t, expected, data)
}

func TestMultiExtentMixedWithRegularFiles(t *testing.T) {
	// Test that multi-extent files and regular files coexist properly.
	// Directory layout:
	//   NORMAL.TXT;1  - regular file at sector 19, 30 bytes
	//   MULTI.BIN;1   - multi-extent file: sector 22 (80 bytes) + sector 23 (40 bytes)

	normalData := []byte("This is a normal file content.")
	me1Data := bytes.Repeat([]byte("X"), 80)
	me2Data := bytes.Repeat([]byte("Y"), 40)

	dotEntry := DirectoryEntry{
		ExtentLocation:       18,
		ExtentLength:         sectorSize,
		FileFlags:            dirFlagDir,
		VolumeSequenceNumber: 1,
		Identifier:           string([]byte{0}),
		SystemUse:            []byte{},
	}
	dotDotEntry := DirectoryEntry{
		ExtentLocation:       18,
		ExtentLength:         sectorSize,
		FileFlags:            dirFlagDir,
		VolumeSequenceNumber: 1,
		Identifier:           string([]byte{1}),
		SystemUse:            []byte{},
	}
	normalFile := DirectoryEntry{
		ExtentLocation:       19,
		ExtentLength:         uint32(len(normalData)),
		FileFlags:            0,
		VolumeSequenceNumber: 1,
		Identifier:           "NORMAL.TXT;1",
		SystemUse:            []byte{},
	}
	multiPart1 := DirectoryEntry{
		ExtentLocation:       22,
		ExtentLength:         80,
		FileFlags:            dirFlagMultiExtent,
		VolumeSequenceNumber: 1,
		Identifier:           "MULTI.BIN;1",
		SystemUse:            []byte{},
	}
	multiPart2 := DirectoryEntry{
		ExtentLocation:       23,
		ExtentLength:         40,
		FileFlags:            0, // final
		VolumeSequenceNumber: 1,
		Identifier:           "MULTI.BIN;1",
		SystemUse:            []byte{},
	}

	entries := []DirectoryEntry{dotEntry, dotDotEntry, normalFile, multiPart1, multiPart2}
	fileData := map[int32][]byte{
		19: normalData,
		22: me1Data,
		23: me2Data,
	}

	img := buildTestISO(t, entries, fileData)

	image, err := OpenImage(bytes.NewReader(img))
	require.NoError(t, err)

	rootDir, err := image.RootDir()
	require.NoError(t, err)

	children, err := rootDir.GetChildren()
	require.NoError(t, err)
	require.Len(t, children, 2)

	// Normal file
	normal := children[0]
	assert.Equal(t, "NORMAL.TXT", normal.Name())
	assert.Equal(t, int64(len(normalData)), normal.Size())
	normalRead, err := io.ReadAll(normal.Reader())
	require.NoError(t, err)
	assert.Equal(t, normalData, normalRead)

	// Multi-extent file
	multi := children[1]
	assert.Equal(t, "MULTI.BIN", multi.Name())
	assert.Equal(t, int64(120), multi.Size())
	multiRead, err := io.ReadAll(multi.Reader())
	require.NoError(t, err)

	expected := make([]byte, 0, 120)
	expected = append(expected, me1Data...)
	expected = append(expected, me2Data...)
	assert.Equal(t, expected, multiRead)
}

func TestSingleExtentFileUnchanged(t *testing.T) {
	// Ensure single-extent files still work correctly (no extents slice set).
	fileContent := []byte("Hello, single extent world!")

	dotEntry := DirectoryEntry{
		ExtentLocation:       18,
		ExtentLength:         sectorSize,
		FileFlags:            dirFlagDir,
		VolumeSequenceNumber: 1,
		Identifier:           string([]byte{0}),
		SystemUse:            []byte{},
	}
	dotDotEntry := DirectoryEntry{
		ExtentLocation:       18,
		ExtentLength:         sectorSize,
		FileFlags:            dirFlagDir,
		VolumeSequenceNumber: 1,
		Identifier:           string([]byte{1}),
		SystemUse:            []byte{},
	}
	singleFile := DirectoryEntry{
		ExtentLocation:       19,
		ExtentLength:         uint32(len(fileContent)),
		FileFlags:            0,
		VolumeSequenceNumber: 1,
		Identifier:           "HELLO.TXT;1",
		SystemUse:            []byte{},
	}

	entries := []DirectoryEntry{dotEntry, dotDotEntry, singleFile}
	fileData := map[int32][]byte{19: fileContent}

	img := buildTestISO(t, entries, fileData)

	image, err := OpenImage(bytes.NewReader(img))
	require.NoError(t, err)

	rootDir, err := image.RootDir()
	require.NoError(t, err)

	children, err := rootDir.GetChildren()
	require.NoError(t, err)
	require.Len(t, children, 1)

	f := children[0]
	assert.Equal(t, "HELLO.TXT", f.Name())
	assert.Equal(t, int64(len(fileContent)), f.Size())
	assert.Nil(t, f.extents) // no extents for single-extent files

	data, err := io.ReadAll(f.Reader())
	require.NoError(t, err)
	assert.Equal(t, fileContent, data)
}

// Helper to suppress unused import warning for binary package.
var _ = binary.LittleEndian
