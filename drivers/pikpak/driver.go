package pikpak

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
	"github.com/go-resty/resty/v2"
	jsoniter "github.com/json-iterator/go"
	log "github.com/sirupsen/logrus"
)

type PikPak struct {
	model.Storage
	Addition
	RefreshToken string
	AccessToken  string
}

func (d *PikPak) Config() driver.Config {
	return config
}

func (d *PikPak) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *PikPak) Init(ctx context.Context) error {
	return d.login()
}

func (d *PikPak) Drop(ctx context.Context) error {
	return nil
}

func (d *PikPak) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	files, err := d.getFiles(dir.GetID())
	if err != nil {
		return nil, err
	}
	return utils.SliceConvert(files, func(src File) (model.Obj, error) {
		return fileToObj(src), nil
	})
}

func (d *PikPak) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	var resp File
	_, err := d.request(fmt.Sprintf("https://api-drive.mypikpak.com/drive/v1/files/%s?_magic=2021&thumbnail_size=SIZE_LARGE", file.GetID()),
		http.MethodGet, nil, &resp)
	if err != nil {
		return nil, err
	}
	link := model.Link{
		URL: resp.WebContentLink,
	}

	if resp.Kind != "drive#folder"{
		var logger = log.New()
		file ,err := os.OpenFile("log/pikpak.log", os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
		if err == nil{
			logger.Out = file
		}else{
			logger.Infof("Failed to log to file")
		}
		logger.Infof("PikPak://%s|%s|%s", resp.Name, resp.Size, resp.Hash)
	}
	if len(resp.Medias) > 0 && resp.Medias[0].Link.Url != "" {
		log.Debugln("use media link")
		link.URL = resp.Medias[0].Link.Url
	}
	return &link, nil
}

func (d *PikPak) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"kind":      "drive#folder",
			"parent_id": parentDir.GetID(),
			"name":      dirName,
		})
	}, nil)
	return err
}

func (d *PikPak) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files:batchMove", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"ids": []string{srcObj.GetID()},
			"to": base.Json{
				"parent_id": dstDir.GetID(),
			},
		})
	}, nil)
	return err
}

func (d *PikPak) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files/"+srcObj.GetID(), http.MethodPatch, func(req *resty.Request) {
		req.SetBody(base.Json{
			"name": newName,
		})
	}, nil)
	return err
}

func (d *PikPak) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files:batchCopy", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"ids": []string{srcObj.GetID()},
			"to": base.Json{
				"parent_id": dstDir.GetID(),
			},
		})
	}, nil)
	return err
}

func (d *PikPak) Remove(ctx context.Context, obj model.Obj) error {
	_, err := d.request("https://api-drive.mypikpak.com/drive/v1/files:batchTrash", http.MethodPost, func(req *resty.Request) {
		req.SetBody(base.Json{
			"ids": []string{obj.GetID()},
		})
	}, nil)
	return err
}

func (d *PikPak) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	tempFile, err := utils.CreateTempFile(stream.GetReadCloser())
	if err != nil {
		return err
	}
	defer func() {
		_ = tempFile.Close()
		_ = os.Remove(tempFile.Name())
	}()
	// cal sha1
	s := sha1.New()
	_, err = io.Copy(s, tempFile)
	if err != nil {
		return err
	}
	_, err = tempFile.Seek(0, io.SeekStart)
	if err != nil {
		return err
	}
	sha1Str := hex.EncodeToString(s.Sum(nil))
	data := base.Json{
		"kind":        "drive#file",
		"name":        stream.GetName(),
		"size":        stream.GetSize(),
		"hash":        strings.ToUpper(sha1Str),
		"upload_type": "UPLOAD_TYPE_RESUMABLE",
		"objProvider": base.Json{"provider": "UPLOAD_TYPE_UNKNOWN"},
		"parent_id":   dstDir.GetID(),
	}
	res, err := d.request("https://api-drive.mypikpak.com/drive/v1/files", http.MethodPost, func(req *resty.Request) {
		req.SetBody(data)
	}, nil)
	if err != nil {
		return err
	}
	if stream.GetSize() == 0 {
		log.Debugln(string(res))
		return nil
	}
	params := jsoniter.Get(res, "resumable").Get("params")
	endpoint := params.Get("endpoint").ToString()
	endpointS := strings.Split(endpoint, ".")
	endpoint = strings.Join(endpointS[1:], ".")
	accessKeyId := params.Get("access_key_id").ToString()
	accessKeySecret := params.Get("access_key_secret").ToString()
	securityToken := params.Get("security_token").ToString()
	key := params.Get("key").ToString()
	bucket := params.Get("bucket").ToString()
	cfg := &aws.Config{
		Credentials: credentials.NewStaticCredentials(accessKeyId, accessKeySecret, securityToken),
		Region:      aws.String("pikpak"),
		Endpoint:    &endpoint,
	}
	ss, err := session.NewSession(cfg)
	if err != nil {
		return err
	}
	uploader := s3manager.NewUploader(ss)
	input := &s3manager.UploadInput{
		Bucket: &bucket,
		Key:    &key,
		Body:   tempFile,
	}
	_, err = uploader.UploadWithContext(ctx, input)
	return err
}

var _ driver.Driver = (*PikPak)(nil)
