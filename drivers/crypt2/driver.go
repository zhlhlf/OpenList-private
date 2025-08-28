package crypt2

import (
	"context"
	"fmt"
	"io"
    "strconv"
	stdpath "path"
	"strings"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"github.com/OpenListTeam/OpenList/v4/internal/sign"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/http_range"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
	"github.com/OpenListTeam/OpenList/v4/server/common"
	rcCrypt "github.com/rclone/rclone/backend/crypt"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/obscure"
	log "github.com/sirupsen/logrus"
)

type Crypt struct {
	model.Storage
	Addition
	cipher        *rcCrypt.Cipher
	remoteStorage driver.Driver
}

const obfuscatedPrefix = "___Obfuscated___"

func (d *Crypt) Config() driver.Config {
	return config
}

func (d *Crypt) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Crypt) Init(ctx context.Context) error {
	//obfuscate credentials if it's updated or just created
	err := d.updateObfusParm(&d.Password)
	if err != nil {
		return fmt.Errorf("failed to obfuscate password: %w", err)
	}
	err = d.updateObfusParm(&d.Salt)
	if err != nil {
		return fmt.Errorf("failed to obfuscate salt: %w", err)
	}

	d.FileNameEncoding = utils.GetNoneEmpty(d.FileNameEncoding, "base64")

	op.MustSaveDriverStorage(d)

	//need remote storage exist
	storage, err := fs.GetStorage(d.RemotePath, &fs.GetStoragesArgs{})
	if err != nil {
		return fmt.Errorf("can't find remote storage: %w", err)
	}
	d.remoteStorage = storage

	p, _ := strings.CutPrefix(d.Password, obfuscatedPrefix)
	p2, _ := strings.CutPrefix(d.Salt, obfuscatedPrefix)
	config := configmap.Simple{
		"password":                  p,
		"password2":                 p2,
		"filename_encryption":       d.FileNameEnc,
		"directory_name_encryption": strconv.FormatBool(d.DirNameEnc),
		"filename_encoding":         d.FileNameEncoding,
		"pass_bad_blocks":           "",
	}
	c, err := rcCrypt.NewCipher(config)
	if err != nil {
		return fmt.Errorf("failed to create Cipher: %w", err)
	}
	d.cipher = c

	return nil
}

func (d *Crypt) updateObfusParm(str *string) error {
	temp := *str
	if !strings.HasPrefix(temp, obfuscatedPrefix) {
		temp, err := obscure.Obscure(temp)
		if err != nil {
			return err
		}
		temp = obfuscatedPrefix + temp
		*str = temp
	}
	return nil
}

func (d *Crypt) Drop(ctx context.Context) error {
	return nil
}

func (d *Crypt) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	path := dir.GetPath()
	//return d.list(ctx, d.RemotePath, path)
	//remoteFull

	objs, err := fs.List(ctx, d.getPathForRemote(path, true), &fs.ListArgs{NoLog: true, Refresh: args.Refresh})
	// the obj must implement the model.SetPath interface
	// return objs, err
	if err != nil {
		return nil, err
	}

	var result []model.Obj
	for _, obj := range objs {
		if obj.IsDir() {
			name, err := d.getDecryptedName(obj.GetName())
			if err != nil {
				//filter illegal files
				continue
			}
			if !d.ShowHidden && strings.HasPrefix(name, ".") {
				continue
			}
			objRes := model.Object{
				Name:     name,
				Size:     0,
				Modified: obj.ModTime(),
				IsFolder: obj.IsDir(),
				Ctime:    obj.CreateTime(),
				// discarding hash as it's encrypted
			}
			result = append(result, &objRes)
		} else {
			thumb, ok := model.GetThumb(obj)
			size, err := d.cipher.DecryptedSize(obj.GetSize())
			// 如果不进行加密文件 读取的大小应该不进行解密
			if d.NoEncryptedFile {
				size = obj.GetSize()
			} else {
				size, err = d.cipher.DecryptedSize(obj.GetSize())
				if err != nil {
					log.Warnf("DecryptedSize failed for %s ,will use original size, err:%s", path, err)
					size = obj.GetSize()
				}
			}
			name, err := d.getDecryptedName(obj.GetName())
			if err != nil {
				//filter illegal files
				continue
			}
			if !d.ShowHidden && strings.HasPrefix(name, ".") {
				continue
			}
			objRes := model.Object{
				Name:     name,
				Size:     size,
				Modified: obj.ModTime(),
				IsFolder: obj.IsDir(),
				Ctime:    obj.CreateTime(),
				// discarding hash as it's encrypted
			}
			if d.Thumbnail && thumb == "" {
				thumbPath := stdpath.Join(args.ReqPath, ".thumbnails", name+".webp")
				thumb = fmt.Sprintf("%s/d%s?sign=%s",
					common.GetApiUrl(ctx),
					utils.EncodePath(thumbPath, true),
					sign.Sign(thumbPath))
			}
			if !ok && !d.Thumbnail {
				result = append(result, &objRes)
			} else {
				objWithThumb := model.ObjThumb{
					Object: objRes,
					Thumbnail: model.Thumbnail{
						Thumbnail: thumb,
					},
				}
				result = append(result, &objWithThumb)
			}
		}
	}

	return result, nil
}

func (d *Crypt) Get(ctx context.Context, path string) (model.Obj, error) {
	if utils.PathEqual(path, "/") {
		return &model.Object{
			Name:     "Root",
			IsFolder: true,
			Path:     "/",
		}, nil
	}
	remoteFullPath := ""
	var remoteObj model.Obj
	var err, err2 error
	firstTryIsFolder, secondTry := guessPath(path)
	remoteFullPath = d.getPathForRemote(path, firstTryIsFolder)
	remoteObj, err = fs.Get(ctx, remoteFullPath, &fs.GetArgs{NoLog: true})
	if err != nil {
		if errs.IsObjectNotFound(err) && secondTry {
			//try the opposite
			remoteFullPath = d.getPathForRemote(path, !firstTryIsFolder)
			remoteObj, err2 = fs.Get(ctx, remoteFullPath, &fs.GetArgs{NoLog: true})
			if err2 != nil {
				return nil, err2
			}
		} else {
			return nil, err
		}
	}
	var size int64 = 0
	name := ""
	if !remoteObj.IsDir() {
		// 如果不进行加密文件 读取的大小应该不进行解密
		if d.NoEncryptedFile {
			size = remoteObj.GetSize()
		} else {
			size, err = d.cipher.DecryptedSize(remoteObj.GetSize())
			if err != nil {
				log.Warnf("DecryptedSize failed for %s ,will use original size, err:%s", path, err)
				size = remoteObj.GetSize()
			}
		}

		name, err = d.getDecryptedName(remoteObj.GetName())

		if err != nil {
			log.Warnf("DecryptFileName failed for %s ,will use original name, err:%s", path, err)
			name = remoteObj.GetName()
		}
	} else {
		name, err = d.getDecryptedName(remoteObj.GetName())
		if err != nil {
			log.Warnf("DecryptDirName failed for %s ,will use original name, err:%s", path, err)
			name = remoteObj.GetName()
		}
	}
	obj := &model.Object{
		Path:     path,
		Name:     name,
		Size:     size,
		Modified: remoteObj.ModTime(),
		IsFolder: remoteObj.IsDir(),
	}
	return obj, nil
	//return nil, errs.ObjectNotFound
}

// https://github.com/rclone/rclone/blob/v1.67.0/backend/crypt/cipher.go#L37
const fileHeaderSize = 32

func (d *Crypt) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	dstDirActualPath, err := d.getActualPathForRemote(file.GetPath(), false)
	if err != nil {
		return nil, fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	remoteLink, remoteFile, err := op.Link(ctx, d.remoteStorage, dstDirActualPath, args)
	if err != nil {
		return nil, err
	}
	if(d.NoEncryptedFile) {
		return remoteLink, nil
	}
	if remoteLink.RangeReadCloser == nil && remoteLink.MFile == nil && len(remoteLink.URL) == 0 {
		return nil, fmt.Errorf("the remote storage driver need to be enhanced to support encrytion")
	}
	resultRangeReadCloser := &model.RangeReadCloser{}
	resultRangeReadCloser.TryAdd(remoteLink.MFile)
	if remoteLink.RangeReadCloser != nil {
		resultRangeReadCloser.AddClosers(remoteLink.RangeReadCloser.GetClosers())
	}
	remoteFileSize := remoteFile.GetSize()
	rangeReaderFunc := func(ctx context.Context, underlyingOffset, underlyingLength int64) (io.ReadCloser, error) {
		length := underlyingLength
		if underlyingLength >= 0 && underlyingOffset+underlyingLength >= remoteFileSize {
			length = -1
		}
		if remoteLink.MFile != nil {
			_, err := remoteLink.MFile.Seek(underlyingOffset, io.SeekStart)
			if err != nil {
				return nil, err
			}
			//keep reuse same MFile and close at last.
			return io.NopCloser(remoteLink.MFile), nil
		}
		rrc := remoteLink.RangeReadCloser
		if rrc == nil && len(remoteLink.URL) > 0 {
			var err error
			rrc, err = stream.GetRangeReadCloserFromLink(remoteFileSize, remoteLink)
			if err != nil {
				return nil, err
			}
			resultRangeReadCloser.AddClosers(rrc.GetClosers())
			remoteLink.RangeReadCloser = rrc
		}
		if rrc != nil {
			remoteReader, err := rrc.RangeRead(ctx, http_range.Range{Start: underlyingOffset, Length: length})
			if err != nil {
				return nil, err
			}
			return remoteReader, nil
		}
		return nil, errs.NotSupport

	}
	resultRangeReadCloser.RangeReader = func(ctx context.Context, httpRange http_range.Range) (io.ReadCloser, error) {
		readSeeker, err := d.cipher.DecryptDataSeek(ctx, rangeReaderFunc, httpRange.Start, httpRange.Length)
		if err != nil {
			return nil, err
		}
		return readSeeker, nil
	}

	return &model.Link{
		RangeReadCloser: resultRangeReadCloser,
	}, nil
}

func (d *Crypt) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	dstDirActualPath, err := d.getActualPathForRemote(parentDir.GetPath(), true)
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	dir, err := d.getEncryptedDirName(dirName)
	return op.MakeDir(ctx, d.remoteStorage, stdpath.Join(dstDirActualPath, dir))
}

func (d *Crypt) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	srcRemoteActualPath, err := d.getActualPathForRemote(srcObj.GetPath(), srcObj.IsDir())
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	dstRemoteActualPath, err := d.getActualPathForRemote(dstDir.GetPath(), dstDir.IsDir())
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	return op.Move(ctx, d.remoteStorage, srcRemoteActualPath, dstRemoteActualPath)
}

func (d *Crypt) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	remoteActualPath, err := d.getActualPathForRemote(srcObj.GetPath(), srcObj.IsDir())
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	var newEncryptedName string
	if srcObj.IsDir() {
		newEncryptedName, err = d.getEncryptedDirName(newName)
	} else {
		newEncryptedName, err = d.getEncryptedName(newName)
	}
	if err != nil {
		return fmt.Errorf("failed to get encrypted name: %w", err)
	}
	return op.Rename(ctx, d.remoteStorage, remoteActualPath, newEncryptedName)
}

func (d *Crypt) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	srcRemoteActualPath, err := d.getActualPathForRemote(srcObj.GetPath(), srcObj.IsDir())
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	dstRemoteActualPath, err := d.getActualPathForRemote(dstDir.GetPath(), dstDir.IsDir())
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	return op.Copy(ctx, d.remoteStorage, srcRemoteActualPath, dstRemoteActualPath)

}

func (d *Crypt) Remove(ctx context.Context, obj model.Obj) error {
	remoteActualPath, err := d.getActualPathForRemote(obj.GetPath(), obj.IsDir())
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	return op.Remove(ctx, d.remoteStorage, remoteActualPath)
}

func (d *Crypt) Put(ctx context.Context, dstDir model.Obj, streamer model.FileStreamer, up driver.UpdateProgress) error {
	
	dstDirActualPath, err := d.getActualPathForRemote(dstDir.GetPath(), true)
	if err != nil {
		return fmt.Errorf("failed to convert path to remote path: %w", err)
	}
	name, err := d.getEncryptedName(streamer.GetName())
	if err != nil {
		return fmt.Errorf("failed to get encrypted name: %w", err)
	}
	if d.NoEncryptedFile {
		streamOut := &stream.FileStream{
		    Obj: &model.Object{
		    	ID:       streamer.GetID(),
		    	Path:     streamer.GetPath(),
		    	Name:     name,
				Size:     streamer.GetSize(),
		    	Modified: streamer.ModTime(),
		    	IsFolder: streamer.IsDir(),
		    },
		    Reader:            streamer,
		    Mimetype:          "application/octet-stream",
		    WebPutAsTask:      streamer.NeedStore(),
		    ForceStreamUpload: true,
		    Exist:             streamer.GetExist(),
	    }
		err = op.Put(ctx, d.remoteStorage, dstDirActualPath, streamOut, up, false)
		if err != nil {
			return err
		} else {
			return nil
		}
	}
	// Encrypt the data into wrappedIn
	wrappedIn, err := d.cipher.EncryptData(streamer)
	if err != nil {
		return fmt.Errorf("failed to EncryptData: %w", err)
	}

	// doesn't support seekableStream, since rapid-upload is not working for encrypted data
	streamOut := &stream.FileStream{
		Obj: &model.Object{
			ID:       streamer.GetID(),
			Path:     streamer.GetPath(),
			Name:     name,
			Size:     d.cipher.EncryptedSize(streamer.GetSize()),
			Modified: streamer.ModTime(),
			IsFolder: streamer.IsDir(),
		},
		Reader:            wrappedIn,
		Mimetype:          "application/octet-stream",
		WebPutAsTask:      streamer.NeedStore(),
		ForceStreamUpload: true,
		Exist:             streamer.GetExist(),
	}
	err = op.Put(ctx, d.remoteStorage, dstDirActualPath, streamOut, up, false)
	if err != nil {
		return err
	}
	return nil
}

//func (d *Safe) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
//	return nil, errs.NotSupport
//}

var _ driver.Driver = (*Crypt)(nil)
