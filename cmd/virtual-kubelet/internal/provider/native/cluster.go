package native

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/kok-stack/native-kubelet/cmd/virtual-kubelet/internal/provider"
	"github.com/kok-stack/native-kubelet/log"
	"github.com/kok-stack/native-kubelet/node/api"
	"github.com/kok-stack/native-kubelet/trace"
	"github.com/pkg/errors"
	"github.com/prologic/bitcask"
	"io"
	"io/ioutil"
	v1 "k8s.io/api/core/v1"
	errors2 "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	corev1listers "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/metrics/pkg/client/clientset/versioned"
	"os"
	"path/filepath"
	"time"
)

const (
	namespaceKey     = "namespace"
	nameKey          = "name"
	containerNameKey = "containerName"
	nodeNameKey      = "nodeName"
	DbPath           = "data"
	ImagePath        = "images"
)

type config struct {
	WorkDir    string `json:"work_dir"`
	MaxTimeout int    `json:"max_timeout"`
}

type Provider struct {
	initConfig provider.InitConfig
	startTime  time.Time
	config     *config

	podHandler     *PodEventHandler
	nodeHandler    *NodeEventHandler
	imageManager   *ImageManager
	db             *bitcask.Bitcask
	processManager *ProcessManager
}

func (p *Provider) NotifyPods(ctx context.Context, f func(*v1.Pod)) {
	_, span := trace.StartSpan(ctx, "Provider.NotifyPods")
	defer span.End()
	p.podHandler.notifyFunc = f
	p.podHandler.start(ctx)
}

func (p *Provider) CreatePod(ctx context.Context, pod *v1.Pod) error {
	ctx, span := trace.StartSpan(ctx, "Provider.CreatePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name, nodeNameKey, p.initConfig.NodeName)
	log.G(ctx).Info("开始创建pod")

	namespace := pod.Namespace
	secrets := getSecrets(pod)
	configMaps := getConfigMaps(pod)
	timeCtx, cancelFunc := context.WithTimeout(ctx, time.Second*10)
	defer cancelFunc()
	if err2 := wait.PollImmediateUntil(time.Microsecond*100, func() (done bool, err error) {
		//TODO:serviceAccount,pvc处理
		//TODO:如何处理pod依赖的对象 serviceAccount-->role-->rolebinding,pvc-->pv-->storageClass,以及其他一些隐试依赖
		for s := range secrets {
			if err := p.syncSecret(ctx, s, namespace); err != nil {
				return false, err
			}
		}
		for s := range configMaps {
			if err := p.syncConfigMap(ctx, s, namespace); err != nil {
				return false, err
			}
		}
		return true, nil
	}, timeCtx.Done()); err2 != nil {
		return err2
	}

	//trimPod(pod, p.initConfig.NodeName)
	//TODO:放到本地存储中
	p.processManager.create(ctx, pod)

	_, err := p.downClientSet.CoreV1().Pods(pod.GetNamespace()).Create(ctx, pod, v12.CreateOptions{})
	if err != nil {
		log.G(ctx).Warnf("创建pod失败", err)
		span.SetStatus(err)
	}
	return err
}

func (p *Provider) UpdatePod(ctx context.Context, pod *v1.Pod) error {
	//up-->down
	ctx, span := trace.StartSpan(ctx, "Provider.UpdatePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name, nodeNameKey, p.initConfig.NodeName)

	trimPod(pod, p.initConfig.NodeName)
	_, err := p.downClientSet.CoreV1().Pods(pod.GetNamespace()).Update(ctx, pod, v12.UpdateOptions{})
	if err != nil {
		span.Logger().Error("更新pod错误", err.Error())
		span.SetStatus(err)
	}
	return nil
}

func (p *Provider) DeletePod(ctx context.Context, pod *v1.Pod) error {
	//up-->down
	ctx, span := trace.StartSpan(ctx, "Provider.DeletePod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, pod.Namespace, nameKey, pod.Name, nodeNameKey, p.initConfig.NodeName)

	err := p.downClientSet.CoreV1().Pods(pod.GetNamespace()).Delete(ctx, pod.GetName(), v12.DeleteOptions{})
	if (err != nil && errors2.IsNotFound(err)) || err == nil {
		return nil
	} else {
		span.Logger().Error("删除pod错误", err.Error())
		span.SetStatus(err)
	}
	return err
}

func (p *Provider) GetPod(ctx context.Context, namespace, name string) (*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetPod")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name, nodeNameKey, p.initConfig.NodeName)

	pod, err := p.initConfig.ResourceManager.GetPod(namespace, name)
	if err != nil {
		log.G(ctx).Warnf("获取up 集群pod出现错误:", err)
		return nil, err
	}
	down, err := p.downPodLister.Pods(namespace).Get(name)
	if err != nil {
		log.G(ctx).Warnf("获取down 集群pod出现错误:", err)
		return nil, err
	}
	pod.Status = down.Status
	return pod, nil
}

func (p *Provider) GetPodStatus(ctx context.Context, namespace, name string) (*v1.PodStatus, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetPodStatus")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, name, nodeNameKey, p.initConfig.NodeName)

	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		span.Logger().Error("获取pod出现错误", err.Error())
		span.SetStatus(err)
		return nil, err
	}
	return &pod.Status, nil
}

func (p *Provider) GetPods(ctx context.Context) ([]*v1.Pod, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetPods")
	defer span.End()
	ctx = addAttributes(ctx, span, nodeNameKey, p.initConfig.NodeName)

	//获取down集群的virtual-kubelet的pod,然后转换status到up集群
	list, err := p.downPodLister.List(labels.Everything())
	if err != nil {
		return nil, err
	}
	pods := make([]*v1.Pod, len(list))
	for i, pod := range list {
		getPod, err := p.initConfig.ResourceManager.GetPod(pod.Namespace, pod.Name)
		if err != nil {
			span.Logger().WithField(namespaceKey, pod.Namespace).WithField(nameKey, pod.Name).Error(err)
			span.SetStatus(err)
			return nil, err
		}
		getPod.Status = pod.Status
		pods[i] = getPod
	}
	return pods, nil
}

func (p *Provider) GetContainerLogs(ctx context.Context, namespace, podName, containerName string, opts api.ContainerLogOpts) (io.ReadCloser, error) {
	ctx, span := trace.StartSpan(ctx, "Provider.GetContainerLogs")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, podName, nodeNameKey, p.initConfig.NodeName)

	tailLine := int64(opts.Tail)
	limitBytes := int64(opts.LimitBytes)
	sinceSeconds := opts.SinceSeconds
	options := &v1.PodLogOptions{
		Container:  containerName,
		Timestamps: opts.Timestamps,
		Follow:     opts.Follow,
	}
	if tailLine != 0 {
		options.TailLines = &tailLine
	}
	if limitBytes != 0 {
		options.LimitBytes = &limitBytes
	}
	if !opts.SinceTime.IsZero() {
		*options.SinceTime = metav1.Time{Time: opts.SinceTime}
	}
	if sinceSeconds != 0 {
		*options.SinceSeconds = int64(sinceSeconds)
	}
	if opts.Previous {
		options.Previous = opts.Previous
	}
	if opts.Follow {
		options.Follow = opts.Follow
	}

	logs := p.downClientSet.CoreV1().Pods(namespace).GetLogs(podName, options)
	stream, err := logs.Stream(ctx)
	if err != nil {
		span.Logger().Error(err.Error())
		span.SetStatus(err)
	}
	return stream, err
}

func (p *Provider) RunInContainer(ctx context.Context, namespace, podName, containerName string, cmd []string, attach api.AttachIO) error {
	ctx, span := trace.StartSpan(ctx, "Provider.RunInContainer")
	defer span.End()
	ctx = addAttributes(ctx, span, namespaceKey, namespace, nameKey, podName, nodeNameKey, p.initConfig.NodeName)

	defer func() {
		if attach.Stdout() != nil {
			attach.Stdout().Close()
		}
		if attach.Stderr() != nil {
			attach.Stderr().Close()
		}
	}()

	req := p.downClientSet.CoreV1().RESTClient().
		Post().
		Namespace(namespace).
		Resource("pods").
		Name(podName).
		SubResource("exec").
		Timeout(0).
		VersionedParams(&v1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdin:     attach.Stdin() != nil,
			Stdout:    attach.Stdout() != nil,
			Stderr:    attach.Stderr() != nil,
			TTY:       attach.TTY(),
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(p.downConfig, "POST", req.URL())
	if err != nil {
		err := fmt.Errorf("could not make remote command: %v", err)
		span.Logger().Error(err.Error())
		span.SetStatus(err)
		return err
	}

	ts := &termSize{attach: attach}

	err = exec.Stream(remotecommand.StreamOptions{
		Stdin:             attach.Stdin(),
		Stdout:            attach.Stdout(),
		Stderr:            attach.Stderr(),
		Tty:               attach.TTY(),
		TerminalSizeQueue: ts,
	})
	if err != nil {
		span.Logger().Error(err.Error())
		span.SetStatus(err)
		return err
	}

	return nil
}

func (p *Provider) ConfigureNode(ctx context.Context, node *v1.Node) {
	p.nodeHandler.configureNode(ctx, node)
}

func (p *Provider) start(ctx context.Context) error {
	file, err := ioutil.ReadFile(p.initConfig.ConfigPath)
	if err != nil {
		return err
	}
	p.config = &config{}
	err = json.Unmarshal(file, p.config)
	if err != nil {
		return err
	}
	//TODO:关闭
	db, err := bitcask.Open(filepath.Join(p.config.WorkDir, DbPath))
	if err != nil {
		return err
	}
	go func() {
		select {
		case <-ctx.Done():
			db.Close()
		}
	}()
	p.db = db
	p.imageManager = NewImageManager(filepath.Join(p.config.WorkDir, ImagePath), db)
	p.processManager = newProcessManager(p.imageManager)
	return nil
}

func NewProvider(ctx context.Context, cfg provider.InitConfig) (*Provider, error) {
	p := &Provider{
		initConfig: cfg,
		startTime:  time.Now(),
	}
	if err := p.start(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

func addAttributes(ctx context.Context, span trace.Span, attrs ...string) context.Context {
	if len(attrs)%2 == 1 {
		return ctx
	}
	for i := 0; i < len(attrs); i += 2 {
		ctx = span.WithField(ctx, attrs[i], attrs[i+1])
	}
	return ctx
}
