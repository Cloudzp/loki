package promtail

import (
	"io/ioutil"
	"path/filepath"
	"time"

	"github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/hpcloud/tail"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/model"
	fsnotify "gopkg.in/fsnotify.v1"

	"github.com/grafana/loki/pkg/helpers"
)

var (
	readBytes = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "promtail",
		Name:      "read_bytes_total",
		Help:      "Number of bytes read.",
	}, []string{"path"})
)

const (
	filename = "__filename__"
)

func init() {
	prometheus.MustRegister(readBytes)
}

// Target describes a particular set of logs.
type Target struct {
	logger log.Logger

	handler   EntryHandler
	positions *Positions

	watcher *fsnotify.Watcher
	path    string
	quit    chan struct{}

	tails map[string]*tailer
}

// NewTarget create a new Target.
func NewTarget(logger log.Logger, handler EntryHandler, positions *Positions, path string, labels model.LabelSet) (*Target, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, errors.Wrap(err, "fsnotify.NewWatcher")
	}

	if err := watcher.Add(path); err != nil {
		helpers.LogError("closing watcher", watcher.Close)
		return nil, errors.Wrap(err, "watcher.Add")
	}

	t := &Target{
		logger:    logger,
		watcher:   watcher,
		path:      path,
		handler:   addLabelsMiddleware(labels).Wrap(handler),
		positions: positions,
		quit:      make(chan struct{}),
		tails:     map[string]*tailer{},
	}

	// Fist, we're going to add all the existing files
	fis, err := ioutil.ReadDir(t.path)
	if err != nil {
		return nil, errors.Wrap(err, "ioutil.ReadDir")
	}
	for _, fi := range fis {
		if fi.IsDir() {
			continue
		}

		tailer, err := newTailer(t.logger, t.handler, t.positions, filepath.Join(t.path, fi.Name()))
		if err != nil {
			level.Error(t.logger).Log("msg", "failed to tail file", "error", err)
			continue
		}

		t.tails[fi.Name()] = tailer
	}

	go t.run()
	return t, nil
}

// Stop the target.
func (t *Target) Stop() {
	close(t.quit)
}

func (t *Target) run() {
	defer func() {
		helpers.LogError("closing watcher", t.watcher.Close)
		for _, v := range t.tails {
			helpers.LogError("stopping tailer", v.stop)
		}
	}()

	for {
		select {
		case event := <-t.watcher.Events:
			switch event.Op {
			case fsnotify.Create:
				// protect against double Creates.
				if _, ok := t.tails[event.Name]; ok {
					level.Info(t.logger).Log("msg", "got 'create' for existing file", "filename", event.Name)
					continue
				}

				tailer, err := newTailer(t.logger, t.handler, t.positions, event.Name)
				if err != nil {
					level.Error(t.logger).Log("msg", "failed to tail file", "error", err)
					continue
				}

				t.tails[event.Name] = tailer

			case fsnotify.Remove:
				tailer, ok := t.tails[event.Name]
				if ok {
					helpers.LogError("stopping tailer", tailer.stop)
					delete(t.tails, event.Name)
				}

			default:
				level.Debug(t.logger).Log("msg", "got unknown event", "event", event)
			}
		case err := <-t.watcher.Errors:
			level.Error(t.logger).Log("msg", "error from fswatch", "error", err)
		case <-t.quit:
			return
		}
	}
}

type tailer struct {
	logger    log.Logger
	handler   EntryHandler

	// TODO 标记文件的读取未知
	positions *Positions

	path string
	tail *tail.Tail
}

func newTailer(logger log.Logger, handler EntryHandler, positions *Positions, path string) (*tailer, error) {

	// tail库： 实时读取文件内容，向linux的 tail 命令一样
	tail, err := tail.TailFile(path, tail.Config{
		Follow: true,
		Location: &tail.SeekInfo{
			Offset: positions.Get(path),
			Whence: 0,
		},
	})
	if err != nil {
		return nil, err
	}

	tailer := &tailer{
		logger:    logger,
		handler:   addLabelsMiddleware(model.LabelSet{filename: model.LabelValue(path)}).Wrap(handler),
		positions: positions,

		path: path,
		tail: tail,
	}
	go tailer.run()
	return tailer, nil
}

func (t *tailer) run() {
	defer func() {
		level.Info(t.logger).Log("msg", "stopping tailing file", "filename", t.path)
	}()

	level.Info(t.logger).Log("msg", "start tailing file", "filename", t.path)
	positionSyncPeriod := t.positions.cfg.SyncPeriod
	positionWait := time.NewTimer(positionSyncPeriod)
	defer positionWait.Stop()

	for {
		select {
        // 在一个固定时间内如果不能获取到日志内容， 需要读取一次文件的当前行数
		case <-positionWait.C:
			// 返回日志文件的当前位置，行号
			pos, err := t.tail.Tell()
			if err != nil {
				level.Error(t.logger).Log("msg", "error getting tail position", "error", err)
				continue
			}
			// 对于每个文件目录，存在一个map 以文件目录为key，以当前位置为value，并添加了同步锁
			// 将上面读取到的位置设置到map中
			t.positions.Put(t.path, pos)
        // 获取到一行日志
		case line, ok := <-t.tail.Lines:
			if !ok {
				return
			}

			if line.Err != nil {
				level.Error(t.logger).Log("msg", "error reading line", "error", line.Err)
			}

			readBytes.WithLabelValues(t.path).Add(float64(len(line.Text)))
			if err := t.handler.Handle(model.LabelSet{}, line.Time, line.Text); err != nil {
				level.Error(t.logger).Log("msg", "error handling line", "error", err)
			}
		}
	}
}

func (t *tailer) stop() error {
	return t.tail.Stop()
}
