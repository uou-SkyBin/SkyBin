package renter

import (
	"compress/zlib"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/user"
	"path"
	"skybin/core"
	"skybin/provider"
	"strings"
	"time"

	"github.com/klauspost/reedsolomon"
)

type BlockDownloadInfo struct {
	BlockId     string `json:"blockId"`
	ProviderId  string `json:"providerId"`
	Location    string `json:"location"`
	TotalTimeMs int64  `json:"totalTimeMs"`
	Error       string `json:"error,omitempty"`
}

type FileDownloadInfo struct {
	FileId      string               `json:"fileId"`
	Name        string               `json:"name"`
	IsDir       bool                 `json:"isDir"`
	VersionNum  int                  `json:"versionNum"`
	DestPath    string               `json:"destPath"`
	TotalTimeMs int64                `json:"totalTimeMs"`
	Blocks      []*BlockDownloadInfo `json:"blocks"`
}

type DownloadInfo struct {
	TotalTimeMs int64               `json:"totalTimeMs"`
	Files       []*FileDownloadInfo `json:"files"`
}

func (r *Renter) Download(fileId string, destPath string, versionNum *int) (*DownloadInfo, error) {
	file, err := r.GetFile(fileId)
	if err != nil {
		return nil, err
	}
	if file.IsDir && versionNum != nil {
		return nil, errors.New("Cannot give version option with folder download")
	}

	// Download to home directory if no destination given
	if len(destPath) == 0 {
		destPath, err = defaultDownloadLocation(file)
		if err != nil {
			return nil, err
		}
	}
	if file.IsDir {
		return r.downloadDir(file, destPath)
	}
	if len(file.Versions) == 0 {
		return nil, errors.New("File has no versions")
	}

	// Download the latest version by default
	version := &file.Versions[len(file.Versions)-1]
	if versionNum != nil {
		version = findVersion(file, *versionNum)
		if version == nil {
			return nil, fmt.Errorf("Cannot find version %d", *versionNum)
		}
	}
	fileInfo, err := r.downloadFile(file, version, destPath)
	if err != nil {
		return nil, err
	}
	return &DownloadInfo{
		TotalTimeMs: fileInfo.TotalTimeMs,
		Files:       []*FileDownloadInfo{fileInfo},
	}, nil
}

// Downloads a folder tree, including all subfolders and files.
// This may partially succeed, in that some children of the folder may
// be downloaded while others may fail.
func (r *Renter) downloadDir(dir *core.File, destPath string) (*DownloadInfo, error) {
	startTime := time.Now()
	fileInfo, err := r.performDirDownload(dir, destPath)
	if err != nil {
		return nil, err
	}
	endTime := time.Now()
	totalTimeMs := toMilliseconds(endTime.Sub(startTime))
	return &DownloadInfo{
		TotalTimeMs: totalTimeMs,
		Files:       fileInfo,
	}, nil
}

// Downloads a single version of a single file.
func (r *Renter) downloadFile(file *core.File, version *core.Version, destPath string) (*FileDownloadInfo, error) {
	startTime := time.Now()
	blockInfo, err := r.performFileDownload(file, version, destPath)
	if err != nil {
		return nil, err
	}
	endTime := time.Now()
	totalTimeMs := toMilliseconds(endTime.Sub(startTime))
	return &FileDownloadInfo{
		FileId:      file.ID,
		Name:        file.Name,
		IsDir:       false,
		VersionNum:  version.Num,
		DestPath:    destPath,
		TotalTimeMs: totalTimeMs,
		Blocks:      blockInfo,
	}, nil
}

func (r *Renter) performDirDownload(dir *core.File, destPath string) ([]*FileDownloadInfo, error) {
	var fileSummaries []*FileDownloadInfo
	dirInfo, err := mkdir(dir, destPath)
	if err != nil {
		return nil, err
	}
	fileSummaries = append(fileSummaries, dirInfo)
	children := r.findChildren(dir)
	for _, child := range children {
		relPath := strings.TrimPrefix(child.Name, dir.Name+"/")
		fullPath := path.Join(destPath, relPath)
		if child.IsDir {
			dirInfo, err = mkdir(child, fullPath)
			if err != nil {
				return nil, fmt.Errorf("Unable to create folder %s. Error: %s", fullPath, err)
			}
			fileSummaries = append(fileSummaries, dirInfo)
			continue
		}
		if len(child.Versions) == 0 {
			return nil, fmt.Errorf("File %s has no versions to download.", child.Name)
		}
		version := &child.Versions[len(child.Versions)-1]
		fileInfo, err := r.downloadFile(child, version, fullPath)
		if err != nil {
			return nil, err
		}
		fileSummaries = append(fileSummaries, fileInfo)
	}
	return fileSummaries, nil
}

func (r *Renter) performFileDownload(file *core.File, version *core.Version, destPath string) ([]*BlockDownloadInfo, error) {
	var blockInfos []*BlockDownloadInfo
	successes := 0
	failures := 0
	var blockFiles []*os.File
	for i := 0; successes < version.NumDataBlocks && failures <= version.NumParityBlocks; i++ {
		temp, err := ioutil.TempFile("", "skybin_download")
		if err != nil {
			return nil, fmt.Errorf("Cannot create temp file. Error: %s", err)
		}
		defer temp.Close()
		defer os.Remove(temp.Name())
		block := &version.Blocks[i]
		blockInfo := &BlockDownloadInfo{
			BlockId:    block.ID,
			ProviderId: block.Location.ProviderId,
			Location:   block.Location.Addr,
		}
		startTime := time.Now()
		err = r.downloadBlock(file.OwnerID, block, temp)
		endTime := time.Now()
		totalTimeMs := toMilliseconds(endTime.Sub(startTime))
		blockInfo.TotalTimeMs = totalTimeMs
		if err == nil {
			successes++
			blockFiles = append(blockFiles, temp)
		} else {
			r.logger.Printf("Error downloading block %s for file %s from provider %s\n",
				block.ID, file.Name, block.Location.ProviderId)
			r.logger.Println("Error: ", err)
			failures++
			blockFiles = append(blockFiles, nil)
			blockInfo.Error = err.Error()
		}
		blockInfos = append(blockInfos, blockInfo)
	}
	if successes < version.NumDataBlocks {
		return nil, errors.New("Failed to download enough file data blocks.")
	}
	needsReconstruction := failures > 0
	err := r.finishDownload(file, version, destPath, blockFiles, needsReconstruction)
	if err != nil {
		return nil, err
	}
	return blockInfos, nil
}

// Completes a file download by reconstructing the file from data and parity blocks (if necessary),
// then decrypting it, decompressing it, and writing it to the destination path.
// blockFiles should be a slice of the files' data and parity blocks, in order,
// with blockFiles[i] set to nil if block i could not be downloaded. The number
// of non-nil elements in blockFiles should equal the number of data blocks in the file.
func (r *Renter) finishDownload(file *core.File, version *core.Version, destPath string,
	blockFiles []*os.File, needsReconstruction bool) error {

	if needsReconstruction {

		// Reconstruct file from parity blocks
		for _, blockFile := range blockFiles {
			if blockFile != nil {
				_, err := blockFile.Seek(0, os.SEEK_SET)
				if err != nil {
					return fmt.Errorf("Unable to seek block file. Error: %s", err)
				}
			}
		}

		blockReaders := convertToReaderSlice(blockFiles)
		for len(blockReaders) < version.NumDataBlocks+version.NumParityBlocks {
			blockReaders = append(blockReaders, nil)
		}

		var fillFiles []*os.File
		for idx, blockReader := range blockReaders {
			var fillFile *os.File = nil
			if blockReader == nil && idx < version.NumDataBlocks {
				temp, err := ioutil.TempFile("", "skybin_download")
				if err != nil {
					return fmt.Errorf("Cannot create temp file. Error: %s", err)
				}
				defer temp.Close()
				defer os.Remove(temp.Name())
				fillFile = temp
			}
			fillFiles = append(fillFiles, fillFile)
		}
		decoder, err := reedsolomon.NewStream(version.NumDataBlocks, version.NumParityBlocks)
		if err != nil {
			return fmt.Errorf("Unable to construct decoder. Error: %s", err)
		}
		err = decoder.Reconstruct(blockReaders, convertToWriterSlice(fillFiles))
		if err != nil {
			return fmt.Errorf("Failed to reconstruct file. Error: %s", err)
		}

		for i := 0; i < version.NumDataBlocks; i++ {
			if blockFiles[i] == nil {
				blockFiles[i] = fillFiles[i]
			}
		}
		blockFiles = blockFiles[:version.NumDataBlocks]
	}

	// Download successful. Rewind the block files.
	if len(blockFiles) != version.NumDataBlocks {
		panic("block files should contain file.NumDataBlocks files")
	}
	for _, f := range blockFiles {
		_, err := f.Seek(0, os.SEEK_SET)
		if err != nil {
			return fmt.Errorf("Unable to seek block file. Error: %s", err)
		}
	}

	// Remove padding of the last block
	if version.PaddingBytes > 0 {
		f := blockFiles[len(blockFiles)-1]
		st, err := f.Stat()
		if err != nil {
			return fmt.Errorf("Unable to stat block file. Error: %s", err)
		}
		err = f.Truncate(st.Size() - version.PaddingBytes)
		if err != nil {
			return fmt.Errorf("Unable to truncate padding bytes. Error: %s", err)
		}
	}

	// Decrypt
	aesKey, aesIV, err := r.decryptEncryptionKeys(file)
	if err != nil {
		return err
	}
	aesCipher, err := aes.NewCipher(aesKey)
	if err != nil {
		return fmt.Errorf("Unable to create aes cipher. Error: %v", err)
	}
	streamReader := cipher.StreamReader{
		S: cipher.NewCFBDecrypter(aesCipher, aesIV),
		R: io.MultiReader(convertToReaderSlice(blockFiles)...),
	}
	temp2, err := ioutil.TempFile("", "skybin_download")
	if err != nil {
		return fmt.Errorf("Unable to create temp file to decrypt download. Error: %v", err)
	}
	defer temp2.Close()
	defer os.Remove(temp2.Name())
	_, err = io.Copy(temp2, streamReader)
	if err != nil {
		return fmt.Errorf("Unable to decrypt file. Error: %s", err)
	}
	_, err = temp2.Seek(0, os.SEEK_SET)
	if err != nil {
		return fmt.Errorf("Unable to seek to beginning of decrypted temp. Error: %s", err)
	}

	// Decompress
	zr, err := zlib.NewReader(temp2)
	if err != nil {
		return fmt.Errorf("Unable to initialize decompression reader. Error: %v", err)
	}
	defer zr.Close()
	outFile, err := os.Create(destPath)
	if err != nil {
		return fmt.Errorf("Unable to create destination file. Error: %v", err)
	}
	defer outFile.Close()
	_, err = io.Copy(outFile, zr)
	if err != nil {
		return fmt.Errorf("Unable to decompress file. Error: %v", err)
	}
	return nil
}

func (r *Renter) downloadBlock(renterId string, block *core.Block, out *os.File) error {
	client := provider.NewClient(block.Location.Addr, &http.Client{})
	blockReader, err := client.GetBlock(renterId, block.ID)
	if err != nil {
		// TODO: Check that failure is due to a network error, not because
		// provider didn't return the block.
		return err
	}
	defer blockReader.Close()
	n, err := io.Copy(out, blockReader)
	if err != nil {
		return fmt.Errorf("Cannot write block to local file. Error: %s", err)
	}
	if n != block.Size {
		return errors.New("Corrupted block: block has incorrect size.")
	}
	_, err = out.Seek(0, os.SEEK_SET)
	if err != nil {
		return fmt.Errorf("Error checking block hash. Error: %s", err)
	}
	h := sha256.New()
	_, err = io.Copy(h, out)
	if err != nil {
		return fmt.Errorf("Error checking block hash. Error: %s", err)
	}
	blockHash := base64.URLEncoding.EncodeToString(h.Sum(nil))
	if blockHash != block.Sha256Hash {
		return errors.New("Corrupted block: block hash does not match that expected.")
	}
	return nil
}

// Decrypts and returns f's AES key and AES IV.
func (r *Renter) decryptEncryptionKeys(f *core.File) (aesKey []byte, aesIV []byte, err error) {
	var keyToDecrypt string
	var ivToDecrypt string

	// If we own the file, use the AES key directly. Otherwise, retrieve them from the relevent permission
	if f.OwnerID == r.Config.RenterId {
		keyToDecrypt = f.AesKey
		ivToDecrypt = f.AesIV
	} else {
		for _, permission := range f.AccessList {
			if permission.RenterId == r.Config.RenterId {
				keyToDecrypt = permission.AesKey
				ivToDecrypt = permission.AesIV
			}
		}
	}

	if keyToDecrypt == "" || ivToDecrypt == "" {
		return nil, nil, errors.New("could not find permission in access list")
	}

	keyBytes, err := base64.URLEncoding.DecodeString(keyToDecrypt)
	if err != nil {
		return nil, nil, err
	}

	ivBytes, err := base64.URLEncoding.DecodeString(ivToDecrypt)
	if err != nil {
		return nil, nil, err
	}

	aesKey, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, r.privKey, keyBytes, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("Unable to decrypt aes key. Error: %v", err)
	}
	aesIV, err = rsa.DecryptOAEP(sha256.New(), rand.Reader, r.privKey, ivBytes, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("Unable to decrypt aes IV. Error: %v", err)
	}
	return aesKey, aesIV, nil
}

func defaultDownloadLocation(f *core.File) (string, error) {
	user, err := user.Current()
	if err != nil {
		return "", err
	}
	destPath := path.Join(user.HomeDir, path.Base(f.Name))
	if _, err := os.Stat(destPath); err == nil {
		for i := 1; ; i++ {
			d := fmt.Sprintf("%s (%d)", destPath, i)
			if _, err := os.Stat(d); os.IsNotExist(err) {
				return d, nil
			}
		}
	}
	return destPath, nil
}

func mkdir(dir *core.File, destPath string) (*FileDownloadInfo, error) {
	err := os.MkdirAll(destPath, 0777)
	if err != nil {
		return nil, err
	}
	return &FileDownloadInfo{
		FileId:   dir.ID,
		Name:     dir.Name,
		IsDir:    true,
		DestPath: destPath,
		Blocks:   []*BlockDownloadInfo{},
	}, nil
}

func convertToWriterSlice(files []*os.File) []io.Writer {
	var res []io.Writer
	for _, f := range files {
		if f == nil {
			// Must explicitly append nil since Go will otherwise
			// not treat f as nil in subsequent equality checks
			res = append(res, nil)
		} else {
			res = append(res, f)
		}

	}
	return res
}

func convertToReaderSlice(files []*os.File) []io.Reader {
	var res []io.Reader
	for _, f := range files {
		if f == nil {
			res = append(res, nil)
		} else {
			res = append(res, f)
		}
	}
	return res
}

func findVersion(file *core.File, versionNum int) *core.Version {
	for i := 0; i < len(file.Versions); i++ {
		if file.Versions[i].Num == versionNum {
			return &file.Versions[i]
		}
	}
	return nil
}

func toMilliseconds(d time.Duration) int64 {
	return int64(d.Seconds() * 1000.0)
}
