package builtins

import (
	"runtime"

	"github.com/docker/docker/api"
	apiserver "github.com/docker/docker/api/server"
	"github.com/docker/docker/daemon/networkdriver/bridge"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/engine"
	"github.com/docker/docker/events"
	"github.com/docker/docker/pkg/parsers/kernel"
	"github.com/docker/docker/registry"
)

//向 eng 对象注册特定的处理 方法。
func Register(eng *engine.Engine) error {
	//向 engine 注册多个 Handler

	//注册网络初始化处理方法
	if err := daemon(eng); err != nil {
		return err
	}
	//注册 API 服务处理方法
	if err := remote(eng); err != nil {
		return err
	}
	//注册 events 事件处理方法
	//给Docker 用户提供 API ，使得用户可以通过这些 API 查看 Docker 内部的 events 信息， log 信息以及 subscribers count 信息
	if err := events.New().Install(eng); err != nil {
		return err
	}
	//注册版本处理方法
	if err := eng.Register("version", dockerVersion); err != nil {
		return err
	}

	//在 eng 对象对外暴露的 API 信息、中添加 docker registry 的信息
	return registry.NewService().Install(eng)
}

// remote: a RESTful api for cross-docker communication
func remote(eng *engine.Engine) error {
	if err := eng.Register("serveapi", apiserver.ServeApi); err != nil {
		return err
	}
	return eng.Register("acceptconnections", apiserver.AcceptConnections)
}

// daemon: a default execution and storage backend for Docker on Linux,
// with the following underlying components:
//
// * Pluggable storage drivers including aufs, vfs, lvm and btrfs.
// * Pluggable execution drivers including lxc and chroot.
//
// In practice `daemon` still includes most core Docker components, including:
//
// * The reference registry client implementation
// * Image management
// * The build facility
// * Logging
//
// These components should be broken off into plugins of their own.
//
func daemon(eng *engine.Engine) error {
	/*向 eng 对象注册处理方法，并不代表处理方法的值函数会被立即调用执
	行，如注册 init networkdrive bridge.lnitDriver 并不会直接运行，而是将 bridge .l nitDriver
	的函数人口作为 init networkdriver 的值，写入 eng handlers 属性中。当 Docker Daemon
	收到名为 init networkdriver Job 的执行请求时， bridge .l nitDriver 才被 Docker Daemon 调用
	执行。*/
	return eng.Register("init_networkdriver", bridge.InitDriver)
}

// builtins jobs independent of any subsystem
func dockerVersion(job *engine.Job) engine.Status {
	v := &engine.Env{}
	v.SetJson("Version", dockerversion.VERSION)
	v.SetJson("ApiVersion", api.APIVERSION)
	v.Set("GitCommit", dockerversion.GITCOMMIT)
	v.Set("GoVersion", runtime.Version())
	v.Set("Os", runtime.GOOS)
	v.Set("Arch", runtime.GOARCH)
	if kernelVersion, err := kernel.GetKernelVersion(); err == nil {
		v.Set("KernelVersion", kernelVersion.String())
	}
	if _, err := v.WriteTo(job.Stdout); err != nil {
		return job.Error(err)
	}
	return engine.StatusOK
}
