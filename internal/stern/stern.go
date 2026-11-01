// Package stern tails logs from many pods and containers at once, selected
// by regex, in the spirit of stern. In follow mode it watches the API so
// pods that appear later, and containers that restart, are picked up
// automatically. Each pod gets its own colour.
package stern

import (
	"bufio"
	"context"
	"fmt"
	"hash/fnv"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"

	"github.com/lyelgate/kubepilot/internal/match"
)

// Options select pods, containers and lines.
type Options struct {
	Namespace      string // "" = all namespaces
	Selector       string // label selector, optional
	Pod            *match.Matcher
	ExcludePod     *match.Matcher // nil = exclude nothing
	Container      *match.Matcher
	Include        []*match.Matcher // a line must match all of these
	Exclude        []*match.Matcher // a line matching any of these is dropped
	Since          time.Duration
	Tail           int64 // -1 = everything
	Follow         bool
	Timestamps     bool
	InitContainers bool
	Color          bool
	MaxStreams     int
}

type tailer struct {
	cs    kubernetes.Interface
	opts  Options
	out   io.Writer
	mu    sync.Mutex // serialises writes
	amu   sync.Mutex
	act   map[string]context.CancelFunc // ns/pod/container -> stop
	ended map[string]time.Time          // when a stream ended, to resume without repeats
	wg    sync.WaitGroup
}

// Run tails until ctx is cancelled (follow) or all logs are printed.
func Run(ctx context.Context, cs kubernetes.Interface, opts Options, out io.Writer) error {
	if opts.MaxStreams == 0 {
		opts.MaxStreams = 100
	}
	t := &tailer{cs: cs, opts: opts, out: out, act: map[string]context.CancelFunc{}, ended: map[string]time.Time{}}
	pods := cs.CoreV1().Pods(opts.Namespace)
	list, err := pods.List(ctx, metav1.ListOptions{LabelSelector: opts.Selector})
	if err != nil {
		return err
	}
	matched := 0
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	for i := range list.Items {
		if t.consider(ctx, &list.Items[i]) {
			matched++
		}
	}
	if !opts.Follow {
		if matched == 0 {
			return fmt.Errorf("no pods match %q", opts.Pod.String())
		}
		t.wg.Wait()
		return nil
	}
	if matched == 0 {
		fmt.Fprintf(out, "Waiting for pods matching %q ...\n", opts.Pod.String())
	}
	rv := list.ResourceVersion
	for ctx.Err() == nil {
		w, err := pods.Watch(ctx, metav1.ListOptions{LabelSelector: opts.Selector, ResourceVersion: rv, AllowWatchBookmarks: true})
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			time.Sleep(2 * time.Second)
			rv = "" // start fresh after an error
			continue
		}
		for ev := range w.ResultChan() {
			pod, ok := ev.Object.(*corev1.Pod)
			if !ok {
				continue // bookmark or status error: re-watch below
			}
			rv = pod.ResourceVersion
			switch ev.Type {
			case watch.Added, watch.Modified:
				t.consider(ctx, pod)
			case watch.Deleted:
				t.stopPod(pod)
			}
		}
		w.Stop()
	}
	t.stopAll()
	t.wg.Wait()
	return nil
}

// consider starts streams for a pod's matching containers; reports a match.
func (t *tailer) consider(ctx context.Context, pod *corev1.Pod) bool {
	if !t.opts.Pod.Match(pod.Name) || (t.opts.ExcludePod != nil && t.opts.ExcludePod.Match(pod.Name)) {
		return false
	}
	type c struct {
		name    string
		running bool
		started bool
	}
	var cs []c
	statuses := map[string]corev1.ContainerStatus{}
	for _, s := range append(append([]corev1.ContainerStatus{}, pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
		statuses[s.Name] = s
	}
	add := func(name string) {
		s := statuses[name]
		cs = append(cs, c{name, s.State.Running != nil, s.State.Running != nil || s.State.Terminated != nil})
	}
	if t.opts.InitContainers {
		for _, ic := range pod.Spec.InitContainers {
			add(ic.Name)
		}
	}
	for _, ctr := range pod.Spec.Containers {
		add(ctr.Name)
	}
	for _, ctr := range cs {
		if !t.opts.Container.Match(ctr.name) {
			continue
		}
		// Follow mode streams running containers; one-shot mode prints any
		// container that has started, including finished ones.
		if (t.opts.Follow && !ctr.running) || (!t.opts.Follow && !ctr.started) {
			continue
		}
		t.start(ctx, pod, ctr.name)
	}
	return true
}

func key(pod *corev1.Pod, container string) string {
	return pod.Namespace + "/" + pod.Name + "/" + container
}

func (t *tailer) start(ctx context.Context, pod *corev1.Pod, container string) {
	k := key(pod, container)
	t.amu.Lock()
	if _, running := t.act[k]; running || len(t.act) >= t.opts.MaxStreams {
		t.amu.Unlock()
		return
	}
	sctx, cancel := context.WithCancel(ctx)
	t.act[k] = cancel
	resume, resumed := t.ended[k]
	t.amu.Unlock()

	lo := &corev1.PodLogOptions{Container: container, Follow: t.opts.Follow, Timestamps: t.opts.Timestamps}
	switch {
	case resumed:
		// The container restarted or the stream dropped: continue from where
		// we stopped instead of replaying its history.
		st := metav1.NewTime(resume)
		lo.SinceTime = &st
	default:
		if t.opts.Since > 0 {
			s := int64(t.opts.Since.Seconds())
			lo.SinceSeconds = &s
		}
		if t.opts.Tail >= 0 {
			n := t.opts.Tail
			lo.TailLines = &n
		}
	}
	prefix := t.prefix(pod, container)
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		defer func() {
			t.amu.Lock()
			delete(t.act, k)
			t.ended[k] = time.Now()
			t.amu.Unlock()
		}()
		stream, err := t.cs.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, lo).Stream(sctx)
		if err != nil {
			if sctx.Err() == nil {
				t.write(prefix, "kp: cannot stream logs: "+err.Error())
			}
			return
		}
		defer stream.Close()
		sc := bufio.NewScanner(stream)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			line := sc.Text()
			if !t.keep(line) {
				continue
			}
			t.write(prefix, line)
		}
	}()
}

func (t *tailer) keep(line string) bool {
	for _, m := range t.opts.Include {
		if !m.Match(line) {
			return false
		}
	}
	for _, m := range t.opts.Exclude {
		if m.Match(line) {
			return false
		}
	}
	return true
}

func (t *tailer) write(prefix, line string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fmt.Fprintf(t.out, "%s %s\n", prefix, line)
}

var palette = []int{31, 32, 33, 34, 35, 36, 91, 92, 93, 94, 95, 96}

func (t *tailer) prefix(pod *corev1.Pod, container string) string {
	name := pod.Name
	if t.opts.Namespace == "" {
		name = pod.Namespace + "/" + pod.Name
	}
	if !t.opts.Color {
		return name + " " + container + " |"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(pod.Namespace + "/" + pod.Name))
	c := palette[int(h.Sum32())%len(palette)]
	return fmt.Sprintf("\x1b[%dm%s\x1b[0m \x1b[2m%s\x1b[0m", c, name, container)
}

func (t *tailer) stopPod(pod *corev1.Pod) {
	t.amu.Lock()
	defer t.amu.Unlock()
	p := pod.Namespace + "/" + pod.Name + "/"
	for k, cancel := range t.act {
		if strings.HasPrefix(k, p) {
			cancel()
		}
	}
}

func (t *tailer) stopAll() {
	t.amu.Lock()
	defer t.amu.Unlock()
	for _, cancel := range t.act {
		cancel()
	}
}
