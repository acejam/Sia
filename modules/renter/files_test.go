package renter

import (
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/renter/siafile"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/NebulousLabs/fastrand"
	"gitlab.com/NebulousLabs/writeaheadlog"
)

// newTestingWal is a helper method to create a wal during testing.
func newTestingWal() *writeaheadlog.WAL {
	walDir := filepath.Join(os.TempDir(), "wals")
	if err := os.MkdirAll(walDir, 0700); err != nil {
		panic(err)
	}
	walPath := filepath.Join(walDir, hex.EncodeToString(fastrand.Bytes(8)))
	_, wal, err := writeaheadlog.New(walPath)
	if err != nil {
		panic(err)
	}
	return wal
}

// newFileTesting is a helper function that calls newFile but returns no error.
//
// Note: Since this function is not a renter method, the file will not be
// properly added to a renter if there is not one in the calling test
func newFileTesting(name string, wal *writeaheadlog.WAL, rsc modules.ErasureCoder, fileSize uint64, mode os.FileMode, source string) *siafile.SiaFile {
	// create the renter/files dir if it doesn't exist
	siaFilePath := filepath.Join(os.TempDir(), "siafiles", name)
	dir, _ := filepath.Split(siaFilePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		panic(err)
	}
	// create the file
	f, err := siafile.New(siaFilePath, name, source, wal, rsc, crypto.GenerateSiaKey(crypto.RandomCipherType()), fileSize, mode)
	if err != nil {
		panic(err)
	}
	return f
}

// newRenterTestFile creates a test file when the test has a renter so that the
// file is properly added to the renter
func (r *Renter) newRenterTestFile() *siafile.SiaFile {
	// Generate name and erasure coding
	name, rsc := testingFileParams()
	// create the renter/files dir if it doesn't exist
	siaFilePath := filepath.Join(r.filesDir, name+siafile.ShareExtension)
	dir, _ := filepath.Split(siaFilePath)
	if err := os.MkdirAll(dir, 0700); err != nil {
		panic(err)
	}
	// Create File
	f, err := r.staticFileSet.NewSiaFile(siaFilePath, name, "", rsc, crypto.GenerateSiaKey(crypto.RandomCipherType()), 1000, 0777, siafile.SiaFileTestThread)
	if err != nil {
		panic(err)
	}
	return f
}

// TestFileNumChunks checks the numChunks method of the file type.
func TestFileNumChunks(t *testing.T) {
	fileSize := func(numSectors uint64) uint64 {
		return numSectors*modules.SectorSize + uint64(fastrand.Intn(int(modules.SectorSize)))
	}
	// Since the pieceSize is 'random' now we test a variety of random inputs.
	tests := []struct {
		fileSize   uint64
		dataPieces int
	}{
		{fileSize(10), 10},
		{fileSize(50), 10},
		{fileSize(100), 10},

		{fileSize(11), 10},
		{fileSize(51), 10},
		{fileSize(101), 10},

		{fileSize(10), 100},
		{fileSize(50), 100},
		{fileSize(100), 100},

		{fileSize(11), 100},
		{fileSize(51), 100},
		{fileSize(101), 100},

		{0, 10}, // 0-length
	}

	for _, test := range tests {
		// Create erasure-coder
		rsc, _ := siafile.NewRSCode(test.dataPieces, 1) // can't use 0
		// Create the file
		f := newFileTesting(t.Name(), newTestingWal(), rsc, test.fileSize, 0777, "")
		// Make sure the file reports the correct pieceSize.
		if f.PieceSize() != modules.SectorSize-f.MasterKey().Type().Overhead() {
			t.Fatal("file has wrong pieceSize for its encryption type")
		}
		// Check that the number of chunks matches the expected number.
		expectedNumChunks := test.fileSize / (f.PieceSize() * uint64(test.dataPieces))
		if expectedNumChunks == 0 {
			// There is at least 1 chunk.
			expectedNumChunks = 1
		} else if expectedNumChunks%(f.PieceSize()*uint64(test.dataPieces)) != 0 {
			// If it doesn't divide evenly there will be 1 chunk padding.
			expectedNumChunks++
		}
		if f.NumChunks() != expectedNumChunks {
			t.Errorf("Test %v: expected %v, got %v", test, expectedNumChunks, f.NumChunks())
		}
	}
}

// TestFileAvailable probes the available method of the file type.
func TestFileAvailable(t *testing.T) {
	rsc, _ := siafile.NewRSCode(1, 1) // can't use 0
	f := newFileTesting(t.Name(), newTestingWal(), rsc, 100, 0777, "")
	neverOffline := make(map[string]bool)

	if f.Available(neverOffline) {
		t.Error("file should not be available")
	}

	for i := uint64(0); i < f.NumChunks(); i++ {
		f.AddPiece(types.SiaPublicKey{}, i, 0, crypto.Hash{})
	}

	if !f.Available(neverOffline) {
		t.Error("file should be available")
	}

	specificOffline := make(map[string]bool)
	specificOffline[string(types.SiaPublicKey{}.Key)] = true
	if f.Available(specificOffline) {
		t.Error("file should not be available")
	}
}

// TestFileUploadedBytes tests that uploadedBytes() returns a value equal to
// the number of sectors stored via contract times the size of each sector.
func TestFileUploadedBytes(t *testing.T) {
	// ensure that a piece fits within a sector
	rsc, _ := siafile.NewRSCode(1, 3)
	f := newFileTesting(t.Name(), newTestingWal(), rsc, 1000, 0777, "")
	for i := uint64(0); i < 4; i++ {
		err := f.AddPiece(types.SiaPublicKey{}, uint64(0), i, crypto.Hash{})
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.UploadedBytes() != 4*modules.SectorSize {
		t.Errorf("expected uploadedBytes to be 8, got %v", f.UploadedBytes())
	}
}

// TestFileUploadProgressPinning verifies that uploadProgress() returns at most
// 100%, even if more pieces have been uploaded,
func TestFileUploadProgressPinning(t *testing.T) {
	rsc, _ := siafile.NewRSCode(1, 1)
	f := newFileTesting(t.Name(), newTestingWal(), rsc, 4, 0777, "")
	for i := uint64(0); i < 2; i++ {
		err1 := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(0)}}, uint64(0), i, crypto.Hash{})
		err2 := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(1)}}, uint64(0), i, crypto.Hash{})
		if err := errors.Compose(err1, err2); err != nil {
			t.Fatal(err)
		}
	}
	if f.UploadProgress() != 100 {
		t.Fatal("expected uploadProgress to report 100% but was", f.UploadProgress())
	}
}

// TestFileRedundancy tests that redundancy is correctly calculated for files
// with varying number of filecontracts and erasure code settings.
func TestFileRedundancy(t *testing.T) {
	nDatas := []int{1, 2, 10}
	neverOffline := make(map[string]bool)
	goodForRenew := make(map[string]bool)
	for i := 0; i < 6; i++ {
		neverOffline[string([]byte{byte(i)})] = false
		goodForRenew[string([]byte{byte(i)})] = true
	}

	for _, nData := range nDatas {
		rsc, _ := siafile.NewRSCode(nData, 10)
		f := newFileTesting(t.Name(), newTestingWal(), rsc, 1000, 0777, "")
		// Test that an empty file has 0 redundancy.
		if r := f.Redundancy(neverOffline, goodForRenew); r != 0 {
			t.Error("expected 0 redundancy, got", r)
		}
		// Test that a file with 1 host that has a piece for every chunk but
		// one chunk still has a redundancy of 0.
		for i := uint64(0); i < f.NumChunks()-1; i++ {
			err := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(0)}}, i, 0, crypto.Hash{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if r := f.Redundancy(neverOffline, goodForRenew); r != 0 {
			t.Error("expected 0 redundancy, got", r)
		}
		// Test that adding another host with a piece for every chunk but one
		// chunk still results in a file with redundancy 0.
		for i := uint64(0); i < f.NumChunks()-1; i++ {
			err := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(1)}}, i, 1, crypto.Hash{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if r := f.Redundancy(neverOffline, goodForRenew); r != 0 {
			t.Error("expected 0 redundancy, got", r)
		}
		// Test that adding a file contract with a piece for the missing chunk
		// results in a file with redundancy > 0 && <= 1.
		err := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(2)}}, f.NumChunks()-1, 0, crypto.Hash{})
		if err != nil {
			t.Fatal(err)
		}
		// 1.0 / MinPieces because the chunk with the least number of pieces has 1 piece.
		expectedR := 1.0 / float64(f.ErasureCode().MinPieces())
		if r := f.Redundancy(neverOffline, goodForRenew); r != expectedR {
			t.Errorf("expected %f redundancy, got %f", expectedR, r)
		}
		// Test that adding a file contract that has erasureCode.MinPieces() pieces
		// per chunk for all chunks results in a file with redundancy > 1.
		for iChunk := uint64(0); iChunk < f.NumChunks(); iChunk++ {
			for iPiece := uint64(1); iPiece < uint64(f.ErasureCode().MinPieces()); iPiece++ {
				err := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(3)}}, iChunk, iPiece, crypto.Hash{})
				if err != nil {
					t.Fatal(err)
				}
			}
			err := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(4)}}, iChunk, uint64(f.ErasureCode().MinPieces()), crypto.Hash{})
			if err != nil {
				t.Fatal(err)
			}
		}
		// 1+MinPieces / MinPieces because the chunk with the least number of pieces has 1+MinPieces pieces.
		expectedR = float64(1+f.ErasureCode().MinPieces()) / float64(f.ErasureCode().MinPieces())
		if r := f.Redundancy(neverOffline, goodForRenew); r != expectedR {
			t.Errorf("expected %f redundancy, got %f", expectedR, r)
		}

		// verify offline file contracts are not counted in the redundancy
		for iChunk := uint64(0); iChunk < f.NumChunks(); iChunk++ {
			for iPiece := uint64(0); iPiece < uint64(f.ErasureCode().MinPieces()); iPiece++ {
				err := f.AddPiece(types.SiaPublicKey{Key: []byte{byte(5)}}, iChunk, iPiece, crypto.Hash{})
				if err != nil {
					t.Fatal(err)
				}
			}
		}
		specificOffline := make(map[string]bool)
		for pk := range goodForRenew {
			specificOffline[pk] = false
		}
		specificOffline[string(byte(5))] = true
		if r := f.Redundancy(specificOffline, goodForRenew); r != expectedR {
			t.Errorf("expected redundancy to ignore offline file contracts, wanted %f got %f", expectedR, r)
		}
	}
}

// TestFileExpiration probes the expiration method of the file type.
func TestFileExpiration(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rsc, _ := siafile.NewRSCode(1, 2)
	f := newFileTesting(t.Name(), newTestingWal(), rsc, 1000, 0777, "")
	contracts := make(map[string]modules.RenterContract)
	if f.Expiration(contracts) != 0 {
		t.Error("file with no pieces should report as having no time remaining")
	}
	// Create 3 public keys
	pk1 := types.SiaPublicKey{Key: []byte{0}}
	pk2 := types.SiaPublicKey{Key: []byte{1}}
	pk3 := types.SiaPublicKey{Key: []byte{2}}

	// Add a piece for each key to the file.
	err1 := f.AddPiece(pk1, 0, 0, crypto.Hash{})
	err2 := f.AddPiece(pk2, 0, 1, crypto.Hash{})
	err3 := f.AddPiece(pk3, 0, 2, crypto.Hash{})
	if err := errors.Compose(err1, err2, err3); err != nil {
		t.Fatal(err)
	}

	// Add a contract.
	fc := modules.RenterContract{}
	fc.EndHeight = 100
	contracts[string(pk1.Key)] = fc
	if f.Expiration(contracts) != 100 {
		t.Error("file did not report lowest WindowStart")
	}

	// Add a contract with a lower WindowStart.
	fc.EndHeight = 50
	contracts[string(pk2.Key)] = fc
	if f.Expiration(contracts) != 50 {
		t.Error("file did not report lowest WindowStart")
	}

	// Add a contract with a higher WindowStart.
	fc.EndHeight = 75
	contracts[string(pk3.Key)] = fc
	if f.Expiration(contracts) != 50 {
		t.Error("file did not report lowest WindowStart")
	}
}

// TestRenterFileListLocalPath verifies that FileList() returns the correct
// local path information for an uploaded file.
func TestRenterFileListLocalPath(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	id := rt.renter.mu.Lock()
	f := rt.renter.newRenterTestFile()
	if err := f.SetLocalPath("TestPath"); err != nil {
		t.Fatal(err)
	}
	rt.renter.mu.Unlock(id)
	files := rt.renter.FileList()
	if len(files) != 1 {
		t.Fatal("wrong number of files, got", len(files), "wanted one")
	}
	if files[0].LocalPath != "TestPath" {
		t.Fatal("file had wrong LocalPath: got", files[0].LocalPath, "wanted TestPath")
	}
}

// TestRenterDeleteFile probes the DeleteFile method of the renter type.
func TestRenterDeleteFile(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Delete a file from an empty renter.
	err = rt.renter.DeleteFile("dne")
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Error("Expected error to contain 'no such file or directory':", err)
	}

	// Put a file in the renter.
	file1 := rt.renter.newRenterTestFile()
	siapath := file1.SiaPath()
	err = rt.renter.staticFileSet.Close(siapath, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Delete a different file.
	err = rt.renter.DeleteFile("one")
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Error("Expected error to contain 'no such file or directory':", err)
	}
	// Delete the file.
	err = rt.renter.DeleteFile(siapath)
	if err != nil {
		t.Error(err)
	}
	if len(rt.renter.FileList()) != 0 {
		t.Error("file was deleted, but is still reported in FileList")
	}
	// Confirm that file was removed from SiaFileSet
	_, err = rt.renter.staticFileSet.Open(siapath, rt.renter.filesDir, siafile.SiaFileTestThread)
	if err == nil {
		t.Fatal("Deleted file still found in staticFileSet")
	}

	// Put a file in the renter, then rename it.
	file2 := rt.renter.newRenterTestFile()
	err = file2.Rename("1", filepath.Join(rt.renter.filesDir, "1"+siafile.ShareExtension)) // set name to "1"
	if err != nil {
		t.Fatal(err)
	}
	siapath2 := file2.SiaPath()
	err = rt.renter.staticFileSet.Close(siapath2, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	err = rt.renter.RenameFile(siapath2, "one")
	if err != nil {
		t.Fatal(err)
	}
	// Call delete on the previous name.
	err = rt.renter.DeleteFile("1")
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Error("Expected error to contain 'no such file or directory':", err)
	}
	// Call delete on the new name.
	err = rt.renter.DeleteFile("one")
	if err != nil {
		t.Error(err)
	}

	// Check that all .sia files have been deleted.
	var walkStr string
	filepath.Walk(rt.renter.filesDir, func(path string, _ os.FileInfo, _ error) error {
		// capture only .sia files
		if filepath.Ext(path) == ".sia" {
			rel, _ := filepath.Rel(rt.renter.filesDir, path) // strip testdir prefix
			walkStr += rel
		}
		return nil
	})
	expWalkStr := ""
	if walkStr != expWalkStr {
		t.Fatalf("Bad walk string: expected %q, got %q", expWalkStr, walkStr)
	}
}

// TestRenterFileList probes the FileList method of the renter type.
func TestRenterFileList(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Get the file list of an empty renter.
	if len(rt.renter.FileList()) != 0 {
		t.Error("FileList has non-zero length for empty renter?")
	}

	// Put a file in the renter.
	file1 := rt.renter.newRenterTestFile()
	if len(rt.renter.FileList()) != 1 {
		t.Error("FileList is not returning the only file in the renter")
	}
	if rt.renter.FileList()[0].SiaPath != file1.SiaPath() {
		t.Error("FileList is not returning the correct filename for the only file")
	}

	// Put multiple files in the renter.
	file2 := rt.renter.newRenterTestFile()
	if len(rt.renter.FileList()) != 2 {
		t.Error("FileList is not returning both files in the renter")
	}
	files := rt.renter.FileList()
	if !((files[0].SiaPath == file1.SiaPath() || files[0].SiaPath == file2.SiaPath()) &&
		(files[1].SiaPath == file1.SiaPath() || files[1].SiaPath == file2.SiaPath()) &&
		(files[0].SiaPath != files[1].SiaPath)) {
		t.Log("files[0].SiaPath", files[0].SiaPath)
		t.Log("files[1].SiaPath", files[1].SiaPath)
		t.Log("file1.SiaPath()", file1.SiaPath())
		t.Log("file2.SiaPath()", file2.SiaPath())
		t.Error("FileList is returning wrong names for the files")
	}
}

// TestRenterRenameFile probes the rename method of the renter.
func TestRenterRenameFile(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Rename a file that doesn't exist.
	err = rt.renter.RenameFile("1", "1a")
	if !strings.Contains(err.Error(), "no such file or directory") {
		t.Error("Expected error to contain 'no such file or directory':", err)
	}

	// Rename a file that does exist.
	f := rt.renter.newRenterTestFile()
	err = f.Rename("1", filepath.Join(rt.renter.filesDir, "1"+siafile.ShareExtension))
	if err != nil {
		t.Fatal(err)
	}
	newName := "1a"
	err = rt.renter.RenameFile("1", newName)
	if err != nil {
		t.Fatal(err)
	}
	files := rt.renter.FileList()
	if len(files) != 1 {
		t.Fatal("FileList has unexpected number of files:", len(files))
	}
	if files[0].SiaPath != newName {
		t.Errorf("RenameFile failed: expected %v, got %v", newName, files[0].SiaPath)
	}
	// Confirm SiaFileSet was updated
	_, err = rt.renter.staticFileSet.Open(newName, rt.renter.filesDir, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal("renter staticFileSet not updated to new file name")
	}
	_, err = rt.renter.staticFileSet.Open("1", rt.renter.filesDir, siafile.SiaFileTestThread)
	if err == nil {
		t.Fatal("old name not removed from renter staticFileSet")
	}

	// Rename a file to an existing name.
	f2 := rt.renter.newRenterTestFile()
	siaPath := f2.SiaPath()
	err = f2.Rename("1", filepath.Join(rt.renter.filesDir, "1"+siafile.ShareExtension))
	if err != nil {
		t.Fatal(err)
	}
	err = rt.renter.staticFileSet.Close(siaPath, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	err = rt.renter.RenameFile("1", newName)
	if err != siafile.ErrPathOverload {
		t.Error("Expecting ErrPathOverload, got", err)
	}

	// Rename a file to the same name.
	err = rt.renter.RenameFile("1", "1")
	if err != siafile.ErrPathOverload {
		t.Error("Expecting ErrPathOverload, got", err)
	}

	// Confirm ability to rename file
	err = rt.renter.RenameFile("1", "1b")
	if err != nil {
		t.Fatal(err)
	}
}

// TestRenterFileDir tests that the renter files are uploaded to the files
// directory and not the root directory of the renter.
func TestRenterFileDir(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Create local file to upload
	localDir := filepath.Join(rt.dir, "files")
	if err := os.MkdirAll(localDir, 0700); err != nil {
		t.Fatal(err)
	}
	size := 100
	fileName := fmt.Sprintf("%dbytes %s", size, hex.EncodeToString(fastrand.Bytes(4)))
	source := filepath.Join(localDir, fileName)
	bytes := fastrand.Bytes(size)
	if err := ioutil.WriteFile(source, bytes, 0600); err != nil {
		t.Fatal(err)
	}

	// Upload local file
	ec, err := siafile.NewRSCode(defaultDataPieces, defaultParityPieces)
	if err != nil {
		t.Fatal(err)
	}
	params := modules.FileUploadParams{
		Source:      source,
		SiaPath:     fileName,
		ErasureCode: ec,
	}
	err = rt.renter.Upload(params)
	if err != nil {
		t.Fatal("failed to upload file:", err)
	}

	// Get file and check siapath
	f, err := rt.renter.File(fileName)
	if err != nil {
		t.Fatal(err)
	}
	if f.SiaPath != fileName {
		t.Fatalf("siapath not set as expected: got %v expected %v", f.SiaPath, fileName)
	}

	// Confirm .sia file exists on disk in the SiapathRoot directory
	renterDir := filepath.Join(rt.dir, modules.RenterDir)
	siapathRootDir := filepath.Join(renterDir, modules.SiapathRoot)
	fullPath := filepath.Join(siapathRootDir, f.SiaPath+".sia")
	if _, err := os.Stat(fullPath); os.IsNotExist(err) {
		t.Fatal("No .sia file found on disk")
	}
}

// TestRenterFilesInMemory confirms that files are added and removed from memory
// as expected when files are in use and not in use
func TestRenterFilesInMemory(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Test adding a file to the renter
	//
	// Add file to renter
	sf1 := rt.renter.newRenterTestFile()
	// Confirm that renter has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Record siaPath
	siaPath := sf1.SiaPath()
	// Close File
	err = rt.renter.staticFileSet.Close(siaPath, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm renter has no files in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 0 {
		t.Fatal("Expected 0 files in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}

	// Test accessing the same file from two separate threads
	//
	// Open file
	sf1, err = rt.renter.staticFileSet.Open(siaPath, rt.renter.filesDir, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Access the file again
	_, err = rt.renter.staticFileSet.Open(siaPath, rt.renter.filesDir, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter still only has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Close one of the file instances
	err = rt.renter.staticFileSet.Close(siaPath, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter still only has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}

	// Confirm closing out remaining files removes all files from memory
	//
	// Close last instance of the first file
	err = rt.renter.staticFileSet.Close(siaPath, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm renter has no files in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 0 {
		t.Fatal("Expected 0 files in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
}

// TestRenterRenameFileInMemory confirms that threads that have access to a file
// will continue to have access to the file even it another thread renames it
func TestRenterRenameFileInMemory(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Test adding a file to the renter
	//
	// Add file to renter
	sf1 := rt.renter.newRenterTestFile()
	// Confirm that renter has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Record siaPath
	siaPath := sf1.SiaPath()

	// Test renaming an instance of a file
	//
	// Access file with another instance
	sf2, err := rt.renter.staticFileSet.Open(siaPath, rt.renter.filesDir, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter still only has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Rename second instance
	newName := "file" + strconv.Itoa(int(fastrand.Bytes(8)[0]))
	err = sf2.Rename(newName, filepath.Join(os.TempDir(), "siafiles", newName))
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter still only has 1 file in memory as renaming doesn't
	// add the new name to memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 files in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Close instance of renamed file
	err = rt.renter.staticFileSet.Close(siaPath, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter still has 1 file1 in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 files in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Close other instance of second file
	err = rt.renter.staticFileSet.Close(siaPath, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm renter has no files in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 0 {
		t.Fatal("Expected 0 files in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
}

// TestRenterDeleteFileInMemory confirms that threads that have access to a file
// will continue to have access to the file even it another thread deletes it
func TestRenterDeleteFileInMemory(t *testing.T) {
	if testing.Short() {
		t.SkipNow()
	}
	rt, err := newRenterTester(t.Name())
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()

	// Test adding a file to the renter
	//
	// Add file to renter
	sf1 := rt.renter.newRenterTestFile()
	// Confirm that renter has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// Record siaPath
	siaPath1 := sf1.SiaPath()

	// Test deleting an instance of a file
	//
	// Access the file again
	sf2, err := rt.renter.staticFileSet.Open(siaPath1, rt.renter.filesDir, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter still only has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// delete and close instance of file
	if err := sf2.Delete(); err != nil {
		t.Fatal(err)
	}
	err = rt.renter.staticFileSet.Close(siaPath1, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm that renter still only has 1 file in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 1 {
		t.Fatal("Expected 1 file in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
	// confirm other instance is still in memory by calling methods on it
	if !sf1.Deleted() {
		t.Fatal("Expected file to be deleted")
	}

	// Confirm closing out remaining files removes all files from memory
	//
	// Close last instance of the first file
	err = rt.renter.staticFileSet.Close(siaPath1, siafile.SiaFileTestThread)
	if err != nil {
		t.Fatal(err)
	}
	// Confirm renter has no files in memory
	if len(rt.renter.staticFileSet.SiaFileMap) != 0 {
		t.Fatal("Expected 0 files in memory, got:", len(rt.renter.staticFileSet.SiaFileMap))
	}
}
