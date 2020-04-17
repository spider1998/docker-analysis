// +build daemon

package main

import (
	"log"

	"github.com/docker/docker/builtins"
	"github.com/docker/docker/daemon"
	_ "github.com/docker/docker/daemon/execdriver/lxc"
	_ "github.com/docker/docker/daemon/execdriver/native"
	"github.com/docker/docker/dockerversion"
	"github.com/docker/docker/engine"
	flag "github.com/docker/docker/pkg/mflag"
	"github.com/docker/docker/pkg/signal"
)

const CanDaemon = true

var (
	daemonCfg = &daemon.Config{}
)

// daemon 的配置初始化。
func init() {
	daemonCfg.InstallFlags()
}

func mainDaemon() {
	//当 docker命令经过 flag 参数解析之后， Docker 判断剩余的参数是否为 0若为 ，则说明 Docker
	//Daemon 的启动命令元误，正常运行;若不为 ，则说明在启动 Docker Daemon 的时候，传
	//人了多余的参数，此时 Docker 会输出错误提示，并退出运行程序
	if flag.NArg() != 0 {
		flag.Usage()
		return
	}

	//创建 engine 对象
	eng := engine.New()

	//设置 engine 的信号捕获
	signal.Trap(eng.Shutdown)
	// Load builtins(Docker Daemon 运行过程中，注册的一些任务(Job) ，这部分任务一般与容器的运行无关，与 Docker Daemon 的运行时信 息有关)
	if err := builtins.Register(eng); err != nil {
		log.Fatal(err)
	}

	// load the daemon in the background so we can immediately start
	// the http api so that connections don't fail while the daemon
	// is booting
	go func() {

		//通过 init 函数中初始化的 daemonCfg eng 对象，创建一个 daemon 对象d
		d, err := daemon.NewDaemon(daemonCfg, eng)
		if err != nil {
			log.Fatal(err)
		}
		//通过 daemon 对象的 Install 函数，向 eng 对象中注册众多的处理方法。
		if err := d.Install(eng); err != nil {
			log.Fatal(err)
		}
		// after the daemon is done setting up we can tell the api to start
		// accepting connections
		//在 Docker Daemon 启动完毕之后，运行名为 acceptconnections Job ，主要工作为向
		//init 守护进程发送阻ADY=l 信号，以便 Docker Se er 开始正常接收请求。
		if err := eng.Job("acceptconnections").Run(); err != nil {
			log.Fatal(err)
		}
	}()
	// TODO actually have a resolved graphdriver to show?
	//打印 Docker 版本及驱动信息
	log.Printf("docker daemon: %s %s; execdriver: %s; graphdriver: %s",
		dockerversion.VERSION,
		dockerversion.GITCOMMIT,
		daemonCfg.ExecDriver,
		daemonCfg.GraphDriver,
	)

	//serveapi 的创建与运行,标志着 Docker Daemon 真正进入状态
	// Serve api
	job := eng.Job("serveapi", flHosts...)
	job.SetenvBool("Logging", true)
	job.SetenvBool("EnableCors", *flEnableCors)
	job.Setenv("Version", dockerversion.VERSION)
	job.Setenv("SocketGroup", *flSocketGroup)

	job.SetenvBool("Tls", *flTls)
	job.SetenvBool("TlsVerify", *flTlsVerify)
	job.Setenv("TlsCa", *flCa)
	job.Setenv("TlsCert", *flCert)
	job.Setenv("TlsKey", *flKey)
	job.SetenvBool("BufferRequests", true)
	if err := job.Run(); err != nil {
		log.Fatal(err)
	}
}
