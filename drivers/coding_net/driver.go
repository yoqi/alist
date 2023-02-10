package codingnet

import (
	"context"
	"github.com/alist-org/alist/v3/drivers/base"
	"github.com/alist-org/alist/v3/pkg/utils"
	"github.com/go-resty/resty/v2"
	"io"
	"net/http"
	"strconv"

	"github.com/alist-org/alist/v3/internal/driver"
	"github.com/alist-org/alist/v3/internal/model"
)

type Coding struct {
	model.Storage
	Addition
	Cookie string
}

func (d *Coding) Config() driver.Config {
	return config
}

func (d *Coding) GetAddition() driver.Additional {
	return &d.Addition
}

func (d *Coding) Init(ctx context.Context) error {
	return d.login()
}

func (d *Coding) Drop(ctx context.Context) error {
	d.Cookie = ""
	return nil
}

func (d *Coding) List(ctx context.Context, dir model.Obj, args model.ListArgs) ([]model.Obj, error) {
	var r DirectoryResp
	err := d.request(http.MethodGet, "/directory"+dir.GetPath(), nil, &r)
	if err != nil {
		return nil, err
	}

	return utils.SliceConvert(r.Objects, func(src Object) (model.Obj, error) {
		return objectToObj(src), nil
	})
}

func (d *Coding) Link(ctx context.Context, file model.Obj, args model.LinkArgs) (*model.Link, error) {
	var dUrl string
	err := d.request(http.MethodPut, "/file/download/"+file.GetID(), nil, &dUrl)
	if err != nil {
		return nil, err
	}
	return &model.Link{
		URL: dUrl,
	}, nil
}

func (d *Coding) MakeDir(ctx context.Context, parentDir model.Obj, dirName string) error {
	return d.request(http.MethodPut, "/directory", func(req *resty.Request) {
		req.SetBody(base.Json{
			"path": parentDir.GetPath() + "/" + dirName,
		})
	}, nil)
}

func (d *Coding) Move(ctx context.Context, srcObj, dstDir model.Obj) error {
	body := base.Json{
		"action":  "move",
		"src_dir": srcObj.GetPath(),
		"dst":     dstDir.GetPath(),
		"src":     convertSrc(srcObj),
	}
	return d.request(http.MethodPatch, "/object", func(req *resty.Request) {
		req.SetBody(body)
	}, nil)
}

func (d *Coding) Rename(ctx context.Context, srcObj model.Obj, newName string) error {
	body := base.Json{
		"action":   "rename",
		"new_name": newName,
		"src":      convertSrc(srcObj),
	}
	return d.request(http.MethodPatch, "/object/rename", func(req *resty.Request) {
		req.SetBody(body)
	}, nil)
}

func (d *Coding) Copy(ctx context.Context, srcObj, dstDir model.Obj) error {
	body := base.Json{
		"src_dir": srcObj.GetPath(),
		"dst":     dstDir.GetPath(),
		"src":     convertSrc(srcObj),
	}
	return d.request(http.MethodPost, "/object/copy", func(req *resty.Request) {
		req.SetBody(body)
	}, nil)
}

func (d *Coding) Remove(ctx context.Context, obj model.Obj) error {
	body := convertSrc(obj)
	err := d.request(http.MethodDelete, "/object", func(req *resty.Request) {
		req.SetBody(body)
	}, nil)
	return err
}

func (d *Coding) Put(ctx context.Context, dstDir model.Obj, stream model.FileStreamer, up driver.UpdateProgress) error {
	if stream.GetReadCloser() == http.NoBody {
		return d.create(ctx, dstDir, stream)
	}
	var r DirectoryResp
	err := d.request(http.MethodGet, "/directory"+dstDir.GetPath(), nil, &r)
	if err != nil {
		return err
	}
	uploadBody := base.Json{
		"path":          dstDir.GetPath(),
		"size":          stream.GetSize(),
		"name":          stream.GetName(),
		"policy_id":     r.Policy.Id,
		"last_modified": stream.ModTime().Unix(),
	}
	var u UploadInfo
	err = d.request(http.MethodPut, "/file/upload", func(req *resty.Request) {
		req.SetBody(uploadBody)
	}, &u)
	if err != nil {
		return err
	}
	var chunkSize = u.ChunkSize
	var buf []byte
	var chunk int
	for {
		var n int
		buf = make([]byte, chunkSize)
		n, err = io.ReadAtLeast(stream, buf, chunkSize)
		if err != nil && err != io.ErrUnexpectedEOF {
			if err == io.EOF {
				return nil
			}
			return err
		}

		if n == 0 {
			break
		}
		buf = buf[:n]
		err = d.request(http.MethodPost, "/file/upload/"+u.SessionID+"/"+strconv.Itoa(chunk), func(req *resty.Request) {
			req.SetHeader("Content-Type", "application/octet-stream")
			req.SetHeader("Content-Length", strconv.Itoa(n))
			req.SetBody(buf)
		}, nil)
		if err != nil {
			break
		}
		chunk++

	}
	return err
}

func (d *Coding) create(ctx context.Context, dir model.Obj, file model.Obj) error {
	body := base.Json{"path": dir.GetPath() + "/" + file.GetName()}
	if file.IsDir() {
		err := d.request(http.MethodPut, "directory", func(req *resty.Request) {
			req.SetBody(body)
		}, nil)
		return err
	}
	return d.request(http.MethodPost, "/file/create", func(req *resty.Request) {
		req.SetBody(body)
	}, nil)
}

//func (d *Coding) Other(ctx context.Context, args model.OtherArgs) (interface{}, error) {
//	return nil, errs.NotSupport
//}

var _ driver.Driver = (*Coding)(nil)
