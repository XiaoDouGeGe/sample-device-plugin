package server

import (
	"context"
	"crypto/md5"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"

	"github.com/fsnotify/fsnotify"
	"google.golang.org/grpc"
	pluginapi "k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta1"
)

const (
	resourceName        string = "myway5.com/cola"
	defaultColaLocation string = "/etc/colas"
	colaSocket          string = "cola.sock"
	KubeletSocket       string = "kubelet.sock"
	DevicePluginPath    string = "/var/lib/kubelet/device-plugins/"
)

type ColaServer struct {
	srv         *grpc.Server
	devices     map[string]*pluginapi.Device
	notify      chan bool
	ctx         context.Context
	cancel      context.CancelFunc
	restartFlag bool
}

func NewColaServer() *ColaServer {
	ctx, cancel := context.WithCancel(context.Background())
	return &ColaServer{
		srv:         grpc.NewServer(grpc.EmptyServerOption{}),
		devices:     make(map[string]*pluginapi.Device),
		notify:      make(chan bool),
		ctx:         ctx,
		cancel:      cancel,
		restartFlag: false,
	}
}

func (s *ColaServer) Run() error {
	err := s.listDevice()
	if err != nil {
		log.Fatalf("list device error: %v", err)
	}

	go func() {
		err := s.watchDevice()
		if err != nil {
			log.Println("watch device error")
		}
	}()

	pluginapi.RegisterDevicePluginServer(s.srv, s)
	err = syscall.Unlink(DevicePluginPath + colaSocket)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	l, err := net.Listen("unix", DevicePluginPath+colaSocket)
	if err != nil {
		return err
	}

	go func() {
		lastCrashTime := time.Now()
		restartCount := 0
		for {
			log.Printf("start GRPC server for '%s'", resourceName)
			err = s.srv.Serve(l)
			if err == nil {
				break
			}

			log.Printf("GRPC server for '%s' crashed with error: %v", resourceName, err)

			if restartCount > 5 {
				log.Fatalf("GRPC server for '%s' has repeatedly crashed recently. Quitting", resourceName)
			}
			timeSinceLastCrash := time.Since(lastCrashTime).Seconds()
			lastCrashTime = time.Now()
			if timeSinceLastCrash > 3600 {
				restartCount = 1
			} else {
				restartCount++
			}
		}
	}()

	conn, err := s.dial(colaSocket, 5*time.Second)
	if err != nil {
		return err
	}
	conn.Close()

	return nil
}

func (s *ColaServer) RegisterToKubelet() error {
	socketFile := filepath.Join(DevicePluginPath + KubeletSocket)

	conn, err := s.dial(socketFile, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pluginapi.NewRegistrationClient(conn)
	req := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     path.Base(DevicePluginPath + colaSocket),
		ResourceName: resourceName,
	}
	log.Infof("Register to kubelet with endpoint %s", req.Endpoint)
	_, err = client.Register(context.Background(), req)
	if err != nil {
		return err
	}

	return nil
}

func (s *ColaServer) GetDevicePluginOptions(ctx context.Context, e *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	log.Infoln("GetDevicePluginOptions called")
	return &pluginapi.DevicePluginOptions{PreStartRequired: true}, nil
}

func (s *ColaServer) ListAndWatch(e *pluginapi.Empty, srv pluginapi.DevicePlugin_ListAndWatchServer) error {
	log.Infoln("ListAndWatch called")
	devs := make([]*pluginapi.Device, len(s.devices))

	i := 0
	for _, dev := range s.devices {
		devs[i] = dev
		i++
	}

	err := srv.Send(&pluginapi.ListAndWatchResponse{Devices: devs})
	if err != nil {
		log.Errorf("ListAndWatch send device error: %v", err)
		return err
	}

	for {
		log.Infoln("waiting for device change")

		select {

		case <-s.notify:
			log.Infoln("开始更新device list, 设备数:", len(s.devices))
			devs := make([]*pluginapi.Device, len(s.devices))

			i := 0
			for _, dev := range s.devices {
				devs[i] = dev
				i++
			}

			srv.Send(&pluginapi.ListAndWatchResponse{Devices: devs})

		case <-s.ctx.Done():
			log.Info("ListAndWatch exit")
			return nil

		}
	}
}

func (s *ColaServer) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	log.Infoln("Allocate called")
	resps := &pluginapi.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
		log.Infof("received request: %v", strings.Join(req.DevicesIDs, ","))
		resp := pluginapi.ContainerAllocateResponse{
			Envs: map[string]string{
				"COLA_DEVICES": strings.Join(req.DevicesIDs, ","),
			},
		}
		resps.ContainerResponses = append(resps.ContainerResponses, &resp)
	}

	return resps, nil
}

func (s *ColaServer) PreStartContainer(ctx context.Context, req *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	log.Infoln("PreStartContainer called")
	return &pluginapi.PreStartContainerResponse{}, nil
}

func (s *ColaServer) listDevice() error {
	dir, err := ioutil.ReadDir(defaultColaLocation)
	if err != nil {
		return err
	}

	for _, f := range dir {
		if f.IsDir() {
			continue
		}

		sum := md5.Sum([]byte(f.Name()))
		s.devices[f.Name()] = &pluginapi.Device{
			ID:     string(sum[:]),
			Health: pluginapi.Healthy,
		}
		log.Infof("find device '%s'", f.Name())
	}

	return nil
}

func (s *ColaServer) watchDevice() error {
	log.Infoln("watching devices")
	w, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("NewWatcher errer: %v", err)
	}
	defer w.Close()

	done := make(chan bool)
	go func() {
		defer func() {
			done <- true
			log.Info("watch device exit")
		}()
		for {
			select {

			case event, ok := <-w.Events:
				if !ok {
					continue
				}
				log.Infoln("device event:", event.Op.String())

				if event.Op&fsnotify.Create == fsnotify.Create {
					sum := md5.Sum([]byte(event.Name))
					s.devices[event.Name] = &pluginapi.Device{
						ID:     string(sum[:]),
						Health: pluginapi.Healthy,
					}
					s.notify <- true
					log.Infoln("new device find:", event.Name)
				} else if event.Op&fsnotify.Remove == fsnotify.Remove {
					delete(s.devices, event.Name)
					s.notify <- true
					log.Infoln("device deleted:", event.Name)
				}

			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				log.Println("error:", err)

			case <-s.ctx.Done():
				break

			}

		}
	}()

	err = w.Add(defaultColaLocation)
	if err != nil {
		return fmt.Errorf("watch device error:%v", err)
	}
	<-done

	return nil
}

func (s *ColaServer) dial(unixSocketPath string, timeout time.Duration) (*grpc.ClientConn, error) {
	c, err := grpc.Dial(unixSocketPath, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(timeout), grpc.WithDialer(
		func(addr string, timeout time.Duration) (net.Conn, error) {
			return net.DialTimeout("unix", addr, timeout)
		}),
	)

	if err != nil {
		return nil, err
	}

	return c, nil
}
