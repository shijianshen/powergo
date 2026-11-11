package tool

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shijianshen/powergo/internal/rpc"
)

// RegisterFS 注册文件系统相关能力。
func RegisterFS(r *Registry) {
	r.Register(&Tool{
		Name: "fs.read",
		Description: "读取文件内容。文本模式按行分页（offset 为起始行号，从 0 开始），" +
			"二进制文件请用 encoding=base64 并按字节偏移读取。",
		Schema: BuildSchema([]Param{
			{Name: "path", Type: "string", Desc: "文件绝对路径", Required: true},
			{Name: "offset", Type: "integer", Desc: "文本模式为起始行号；base64 模式为起始字节偏移", Default: 0},
			{Name: "limit", Type: "integer", Desc: "文本模式为最多行数；base64 模式为最多字节数", Default: 2000},
			{Name: "encoding", Type: "string", Desc: "text 按 UTF-8 文本返回，base64 返回原始字节", Enum: []string{"text", "base64"}, Default: "text"},
		}),
		Handler: fsRead,
	})

	r.Register(&Tool{
		Name:        "fs.write",
		Description: "覆盖写入文件。父目录不存在时默认自动创建。",
		Schema: BuildSchema([]Param{
			{Name: "path", Type: "string", Desc: "文件绝对路径", Required: true},
			{Name: "content", Type: "string", Desc: "写入内容；encoding=base64 时应传 base64 字符串", Required: true},
			{Name: "encoding", Type: "string", Desc: "text 或 base64", Enum: []string{"text", "base64"}, Default: "text"},
			{Name: "create_dirs", Type: "boolean", Desc: "是否自动创建父目录", Default: true},
		}),
		Handler: fsWrite,
	})

	r.Register(&Tool{
		Name:        "fs.append",
		Description: "追加内容到文件末尾，文件不存在时创建。",
		Schema: BuildSchema([]Param{
			{Name: "path", Type: "string", Desc: "文件绝对路径", Required: true},
			{Name: "content", Type: "string", Desc: "追加内容", Required: true},
			{Name: "encoding", Type: "string", Desc: "text 或 base64", Enum: []string{"text", "base64"}, Default: "text"},
		}),
		Handler: fsAppend,
	})

	r.Register(&Tool{
		Name:        "fs.list",
		Description: "列出目录下的直接子项。不递归，需要递归请用 fs.search。",
		Schema: BuildSchema([]Param{
			{Name: "path", Type: "string", Desc: "目录绝对路径", Required: true},
			{Name: "show_hidden", Type: "boolean", Desc: "是否包含隐藏项", Default: false},
		}),
		Handler: fsList,
	})

	r.Register(&Tool{
		Name:        "fs.stat",
		Description: "获取文件或目录的元信息（大小、修改时间、类型）。",
		Schema: BuildSchema([]Param{
			{Name: "path", Type: "string", Desc: "绝对路径", Required: true},
		}),
		Handler: fsStat,
	})

	r.Register(&Tool{
		Name:        "fs.mkdir",
		Description: "创建目录，默认递归创建父目录。",
		Schema: BuildSchema([]Param{
			{Name: "path", Type: "string", Desc: "目录绝对路径", Required: true},
			{Name: "parents", Type: "boolean", Desc: "是否递归创建", Default: true},
		}),
		Handler: fsMkdir,
	})

	r.Register(&Tool{
		Name:        "fs.remove",
		Description: "删除文件或目录。删除非空目录必须显式传 recursive=true。",
		Schema: BuildSchema([]Param{
			{Name: "path", Type: "string", Desc: "绝对路径", Required: true},
			{Name: "recursive", Type: "boolean", Desc: "递归删除目录树", Default: false},
		}),
		Handler: fsRemove,
	})

	r.Register(&Tool{
		Name:        "fs.move",
		Description: "移动或重命名文件/目录，跨盘时自动降级为复制后删除。",
		Schema: BuildSchema([]Param{
			{Name: "src", Type: "string", Desc: "源路径", Required: true},
			{Name: "dst", Type: "string", Desc: "目标路径", Required: true},
			{Name: "overwrite", Type: "boolean", Desc: "目标存在时是否覆盖", Default: false},
		}),
		Handler: fsMove,
	})

	r.Register(&Tool{
		Name:        "fs.copy",
		Description: "复制文件或目录树。",
		Schema: BuildSchema([]Param{
			{Name: "src", Type: "string", Desc: "源路径", Required: true},
			{Name: "dst", Type: "string", Desc: "目标路径", Required: true},
			{Name: "overwrite", Type: "boolean", Desc: "目标存在时是否覆盖", Default: false},
		}),
		Handler: fsCopy,
	})

	r.Register(&Tool{
		Name: "fs.search",
		Description: "递归查找文件。name_pattern 支持通配符（如 *.log），content 会在文件内容里做关键字匹配。" +
			"自动跳过 .git、node_modules 等大目录。",
		Schema: BuildSchema([]Param{
			{Name: "root", Type: "string", Desc: "搜索起始目录", Required: true},
			{Name: "name_pattern", Type: "string", Desc: "文件名通配符，如 *.go"},
			{Name: "content", Type: "string", Desc: "内容关键字，命中则返回该文件"},
			{Name: "max_results", Type: "integer", Desc: "最多返回条数", Default: 200},
			{Name: "max_depth", Type: "integer", Desc: "最大递归深度", Default: 8},
		}),
		Handler: fsSearch,
	})
}

type readResult struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Size       int64  `json:"size"`
	Lines      int    `json:"lines"`
	Offset     int    `json:"offset"`
	Truncated  bool   `json:"truncated"`
	NextOffset int    `json:"next_offset"`
	Encoding   string `json:"encoding"`
}

func fsRead(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Path     string `json:"path"`
		Offset   int    `json:"offset"`
		Limit    int    `json:"limit"`
		Encoding string `json:"encoding"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Path)
	if e != nil {
		return nil, e
	}
	if p.Offset < 0 {
		p.Offset = 0
	}

	f, err := os.Open(abs)
	if err != nil {
		return nil, fsErr(err, "读取", abs)
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return nil, fsErr(err, "读取", abs)
	}
	if st.IsDir() {
		return nil, rpc.ErrInvalidParams("%s 是目录，请改用 fs.list", abs)
	}

	maxBytes := c.Cfg.MaxReadBytes
	if maxBytes <= 0 {
		maxBytes = 4 << 20
	}

	if p.Encoding == "base64" {
		if p.Limit <= 0 || int64(p.Limit) > maxBytes {
			p.Limit = int(maxBytes)
		}
		if _, err := f.Seek(int64(p.Offset), io.SeekStart); err != nil {
			return nil, fsErr(err, "定位", abs)
		}
		buf := make([]byte, p.Limit)
		n, rerr := io.ReadFull(f, buf)
		if rerr != nil && rerr != io.ErrUnexpectedEOF && rerr != io.EOF {
			return nil, rpc.ErrInternal(rerr)
		}
		buf = buf[:n]
		return readResult{
			Path:       abs,
			Content:    base64.StdEncoding.EncodeToString(buf),
			Size:       st.Size(),
			Offset:     p.Offset,
			Truncated:  int64(p.Offset+n) < st.Size(),
			NextOffset: p.Offset + n,
			Encoding:   "base64",
		}, nil
	}

	if p.Limit <= 0 {
		p.Limit = 2000
	}

	br := bufio.NewReaderSize(f, 64<<10)
	for i := 0; i < p.Offset; i++ {
		if _, err := br.ReadString('\n'); err != nil {
			// 偏移超过文件行数，返回空结果而不是报错
			return readResult{Path: abs, Size: st.Size(), Offset: p.Offset, Encoding: "text"}, nil
		}
	}

	var lines []string
	var used int64
	truncated := false

	for len(lines) < p.Limit {
		line, err := br.ReadString('\n')
		if line != "" {
			if int64(len(line)) > maxBytes {
				return nil, rpc.ErrInvalidParams("%s 存在超长行（>%s），请用 encoding=base64", abs, humanBytes(maxBytes))
			}
			lines = append(lines, strings.TrimRight(line, "\r\n"))
			used += int64(len(line))
			if used >= maxBytes {
				truncated = true
				break
			}
		}
		if err != nil {
			break
		}
	}
	if !truncated && len(lines) == p.Limit {
		if _, err := br.ReadString('\n'); err == nil {
			truncated = true
		}
	}

	return readResult{
		Path:       abs,
		Content:    strings.Join(lines, "\n"),
		Size:       st.Size(),
		Lines:      len(lines),
		Offset:     p.Offset,
		Truncated:  truncated,
		NextOffset: p.Offset + len(lines),
		Encoding:   "text",
	}, nil
}

type writeResult struct {
	Path  string `json:"path"`
	Bytes int    `json:"bytes"`
}

func fsWrite(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Path       string `json:"path"`
		Content    string `json:"content"`
		Encoding   string `json:"encoding"`
		CreateDirs *bool  `json:"create_dirs"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Path)
	if e != nil {
		return nil, e
	}

	data := []byte(p.Content)
	if p.Encoding == "base64" {
		var err error
		data, err = base64.StdEncoding.DecodeString(p.Content)
		if err != nil {
			return nil, rpc.ErrInvalidParams("base64 解码失败: %v", err)
		}
	}

	createDirs := p.CreateDirs == nil || *p.CreateDirs
	if createDirs {
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return nil, fsErr(err, "创建父目录", filepath.Dir(abs))
		}
	}

	if err := os.WriteFile(abs, data, 0o644); err != nil {
		return nil, fsErr(err, "写入", abs)
	}
	return writeResult{Path: abs, Bytes: len(data)}, nil
}

func fsAppend(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Path     string `json:"path"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Path)
	if e != nil {
		return nil, e
	}

	data := []byte(p.Content)
	if p.Encoding == "base64" {
		var err error
		data, err = base64.StdEncoding.DecodeString(p.Content)
		if err != nil {
			return nil, rpc.ErrInvalidParams("base64 解码失败: %v", err)
		}
	}

	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, fsErr(err, "打开", abs)
	}
	defer f.Close()

	n, err := f.Write(data)
	if err != nil {
		return nil, fsErr(err, "追加", abs)
	}
	return writeResult{Path: abs, Bytes: n}, nil
}

type entry struct {
	Name    string    `json:"name"`
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
}

type listResult struct {
	Path      string  `json:"path"`
	Count     int     `json:"count"`
	Entries   []entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

func fsList(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Path       string `json:"path"`
		ShowHidden bool   `json:"show_hidden"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Path)
	if e != nil {
		return nil, e
	}

	des, err := os.ReadDir(abs)
	if err != nil {
		return nil, fsErr(err, "列目录", abs)
	}

	const maxEntries = 500
	res := listResult{Path: abs, Entries: make([]entry, 0, len(des))}
	for _, de := range des {
		name := de.Name()
		if !p.ShowHidden && strings.HasPrefix(name, ".") {
			continue
		}
		if len(res.Entries) >= maxEntries {
			res.Truncated = true
			break
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		res.Entries = append(res.Entries, entry{
			Name:    name,
			Path:    filepath.Join(abs, name),
			IsDir:   de.IsDir(),
			Size:    info.Size(),
			ModTime: info.ModTime(),
		})
	}
	res.Count = len(res.Entries)
	return res, nil
}

func fsStat(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Path string `json:"path"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Path)
	if e != nil {
		return nil, e
	}
	st, err := os.Stat(abs)
	if err != nil {
		return nil, fsErr(err, "stat", abs)
	}
	return map[string]interface{}{
		"path":     abs,
		"name":     st.Name(),
		"is_dir":   st.IsDir(),
		"size":     st.Size(),
		"mode":     st.Mode().String(),
		"mod_time": st.ModTime(),
	}, nil
}

func fsMkdir(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Path    string `json:"path"`
		Parents *bool  `json:"parents"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Path)
	if e != nil {
		return nil, e
	}
	parents := p.Parents == nil || *p.Parents
	var err error
	if parents {
		err = os.MkdirAll(abs, 0o755)
	} else {
		err = os.Mkdir(abs, 0o755)
	}
	if err != nil {
		return nil, fsErr(err, "创建目录", abs)
	}
	return map[string]interface{}{"path": abs, "created": true}, nil
}

func fsRemove(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Path)
	if e != nil {
		return nil, e
	}

	st, err := os.Stat(abs)
	if err != nil {
		return nil, fsErr(err, "删除", abs)
	}

	if st.IsDir() {
		if !p.Recursive {
			// 非空目录必须显式确认，避免 AI 一次误调用删掉整棵树
			if err := os.Remove(abs); err != nil {
				return nil, rpc.ErrInvalidParams("目录非空，需显式传 recursive=true 才能递归删除: %s", abs)
			}
			return map[string]interface{}{"path": abs, "removed": true}, nil
		}
		if err := os.RemoveAll(abs); err != nil {
			return nil, fsErr(err, "递归删除", abs)
		}
		return map[string]interface{}{"path": abs, "removed": true, "recursive": true}, nil
	}

	if err := os.Remove(abs); err != nil {
		return nil, fsErr(err, "删除", abs)
	}
	return map[string]interface{}{"path": abs, "removed": true}, nil
}

func fsMove(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Overwrite bool   `json:"overwrite"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	src, e := resolvePath(c, p.Src)
	if e != nil {
		return nil, e
	}
	dst, e := resolvePath(c, p.Dst)
	if e != nil {
		return nil, e
	}

	if _, err := os.Stat(dst); err == nil {
		if !p.Overwrite {
			return nil, rpc.NewError(rpc.CodeConflict, "目标已存在: %s", dst)
		}
		if err := os.RemoveAll(dst); err != nil {
			return nil, fsErr(err, "覆盖目标", dst)
		}
	}

	if err := os.Rename(src, dst); err != nil {
		// 跨盘符时 Rename 会失败，退化为复制 + 删除
		if cerr := copyAny(src, dst); cerr != nil {
			return nil, fsErr(cerr, "移动", src)
		}
		if rerr := os.RemoveAll(src); rerr != nil {
			return nil, fsErr(rerr, "删除源", src)
		}
	}
	return map[string]interface{}{"src": src, "dst": dst, "moved": true}, nil
}

func fsCopy(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Overwrite bool   `json:"overwrite"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	src, e := resolvePath(c, p.Src)
	if e != nil {
		return nil, e
	}
	dst, e := resolvePath(c, p.Dst)
	if e != nil {
		return nil, e
	}

	if _, err := os.Stat(dst); err == nil && !p.Overwrite {
		return nil, rpc.NewError(rpc.CodeConflict, "目标已存在: %s", dst)
	}
	if err := copyAny(src, dst); err != nil {
		return nil, fsErr(err, "复制", src)
	}
	return map[string]interface{}{"src": src, "dst": dst, "copied": true}, nil
}

func copyAny(src, dst string) error {
	st, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(src, dst)
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

type searchHit struct {
	Path    string    `json:"path"`
	IsDir   bool      `json:"is_dir"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mod_time"`
	Matched string    `json:"matched_by"`
}

type searchResult struct {
	Root      string      `json:"root"`
	Count     int         `json:"count"`
	Hits      []searchHit `json:"hits"`
	Truncated bool        `json:"truncated"`
}

var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "$recycle.bin": true,
	"system volume information": true, ".svn": true, "__pycache__": true,
	".venv": true, "venv": true, "windows": true, "appdata": true,
}

func fsSearch(c *Context, params json.RawMessage) (interface{}, *rpc.Error) {
	var p struct {
		Root        string `json:"root"`
		NamePattern string `json:"name_pattern"`
		Content     string `json:"content"`
		MaxResults  int    `json:"max_results"`
		MaxDepth    int    `json:"max_depth"`
	}
	if e := Bind(params, &p); e != nil {
		return nil, e
	}
	abs, e := resolvePath(c, p.Root)
	if e != nil {
		return nil, e
	}
	if p.MaxResults <= 0 {
		p.MaxResults = 200
	}
	if p.MaxDepth <= 0 {
		p.MaxDepth = 8
	}
	if p.NamePattern == "" && p.Content == "" {
		return nil, rpc.ErrInvalidParams("name_pattern 与 content 至少提供一个")
	}

	res := searchResult{Root: abs, Hits: make([]searchHit, 0, 16)}
	baseDepth := strings.Count(filepath.Clean(abs), string(os.PathSeparator))

	_ = filepath.Walk(abs, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			if path == abs {
				return nil
			}
			if skipDirs[strings.ToLower(info.Name())] {
				return filepath.SkipDir
			}
			if strings.Count(filepath.Clean(path), string(os.PathSeparator))-baseDepth > p.MaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if len(res.Hits) >= p.MaxResults {
			res.Truncated = true
			return filepath.SkipAll
		}

		matched := ""
		if p.NamePattern != "" {
			if ok, _ := filepath.Match(p.NamePattern, info.Name()); ok {
				matched = "name"
			}
		}
		if matched == "" && p.Content != "" {
			if hit, _ := fileContains(path, p.Content); hit {
				matched = "content"
			}
		}
		if matched != "" {
			res.Hits = append(res.Hits, searchHit{
				Path: path, IsDir: false, Size: info.Size(),
				ModTime: info.ModTime(), Matched: matched,
			})
		}
		return nil
	})

	res.Count = len(res.Hits)
	return res, nil
}

// fileContains 只扫前 2MB，避免在巨型日志上做全文件关键字匹配。
func fileContains(path, needle string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()

	const scanLimit = 2 << 20
	br := bufio.NewReaderSize(io.LimitReader(f, scanLimit), 64<<10)
	needleLower := strings.ToLower(needle)

	for {
		line, err := br.ReadString('\n')
		if line != "" && strings.Contains(strings.ToLower(line), needleLower) {
			return true, nil
		}
		if err != nil {
			return false, nil
		}
	}
}
