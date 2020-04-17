// +build linux,cgo

package native

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/docker/docker/daemon/execdriver"
	"github.com/docker/docker/daemon/execdriver/native/configuration"
	"github.com/docker/docker/daemon/execdriver/native/template"
	"github.com/docker/libcontainer"
	"github.com/docker/libcontainer/apparmor"
	"github.com/docker/libcontainer/devices"
	"github.com/docker/libcontainer/mount"
	"github.com/docker/libcontainer/security/capabilities"
)

// createContainer populates and configures the container type with the
// data provided by the execdriver.Command
func (d *driver) createContainer(c *execdriver.Command) (*libcontainer.Config, error) {
	container := template.New() //创建一个 libcontainer. Config 的实 例容器

	container.Hostname = getEnv("HOSTNAME", c.Env)
	container.Tty = c.Tty
	container.User = c.User
	container.WorkingDir = c.WorkingDir
	container.Env = c.Env
	container.Cgroups.Name = c.ID
	container.Cgroups.AllowedDevices = c.AllowedDevices
	container.MountConfig.DeviceNodes = c.AutoCreatedDevices

	// check to see if we are running in ramdisk to disable pivot root
	container.MountConfig.NoPivotRoot = os.Getenv("DOCKER_RAMDISK") != ""
	container.RestrictSys = true

	//配置网络环境
	if err := d.createNetwork(container, c); err != nil {
		return nil, err
	}

	if c.Privileged {
		if err := d.setPrivileged(container); err != nil {
			return nil, err
		}
	} else {
		if err := d.setCapabilities(container, c); err != nil {
			return nil, err
		}
	}

	if err := d.setupCgroups(container, c); err != nil {
		return nil, err
	}

	if err := d.setupMounts(container, c); err != nil {
		return nil, err
	}

	if err := d.setupLabels(container, c); err != nil {
		return nil, err
	}

	cmds := make(map[string]*exec.Cmd)
	d.Lock()
	for k, v := range d.activeContainers {
		cmds[k] = v.cmd
	}
	d.Unlock()

	if err := configuration.ParseConfiguration(container, cmds, c.Config["native"]); err != nil {
		return nil, err
	}

	return container, nil
}

//来判断如何创建 libcontainer.Con 中的 Network 属性
func (d *driver) createNetwork(container *libcontainer.Config, c *execdriver.Command) error {
	if c.Network.HostNetworking {
		container.Namespaces["NEWNET"] = false
		return nil
	}

	container.Networks = []*libcontainer.Network{
		{
			Mtu:     c.Network.Mtu,
			Address: fmt.Sprintf("%s/%d", "127.0.0.1", 0),
			Gateway: "localhost",
			Type:    "loopback",
		},
	}

	if c.Network.Interface != nil { //判断容器网络是否为 bridge 桥接模式的源码:
		vethNetwork := libcontainer.Network{
			Mtu:        c.Network.Mtu,
			Address:    fmt.Sprintf("%s/%d", c.Network.Interface.IPAddress, c.Network.Interface.IPPrefixLen),
			Gateway:    c.Network.Interface.Gateway,
			Type:       "veth",
			Bridge:     c.Network.Interface.Bridge,
			VethPrefix: "veth",
		}
		container.Networks = append(container.Networks, &vethNetwork)
	}

	if c.Network.ContainerID != "" { //判断容器网络是否为 other container 模式的代码:
		//execdriver.Command 类型实例中 Network 属性的 ContainerID 不为空字符串时，则说
		//明需要为 Docker 容器创建 other container 模式，使创建容器共享其他容器的网络环境。实
		//现过程中， execdriver 首先需要在 activeContainers 中查找需要被共享网络环境的容器 active ;
		//并通过 active 容器的启动执行命令 cmd 找到容器主进程在宿主机上的 PID; 随后在 proc 文件
		//系统中找到该进程 PID 的关于网络命名空间的路径 nspath ，也是整个容器的网络命名空间路
		//径;最后为类型为 libcontainer.Confìg container 对象添加 Networks 属性， Network 的类型
		//netns

		d.Lock()
		active := d.activeContainers[c.Network.ContainerID]
		d.Unlock()

		if active == nil || active.cmd.Process == nil {
			return fmt.Errorf("%s is not a valid running container to join", c.Network.ContainerID)
		}
		cmd := active.cmd

		nspath := filepath.Join("/proc", fmt.Sprint(cmd.Process.Pid), "ns", "net")
		container.Networks = append(container.Networks, &libcontainer.Network{
			Type:   "netns",
			NsPath: nspath,
		})
	}

	return nil
}

func (d *driver) setPrivileged(container *libcontainer.Config) (err error) {
	container.Capabilities = capabilities.GetAllCapabilities()
	container.Cgroups.AllowAllDevices = true

	hostDeviceNodes, err := devices.GetHostDeviceNodes()
	if err != nil {
		return err
	}
	container.MountConfig.DeviceNodes = hostDeviceNodes

	container.RestrictSys = false

	if apparmor.IsEnabled() {
		container.AppArmorProfile = "unconfined"
	}

	return nil
}

func (d *driver) setCapabilities(container *libcontainer.Config, c *execdriver.Command) (err error) {
	container.Capabilities, err = execdriver.TweakCapabilities(container.Capabilities, c.CapAdd, c.CapDrop)
	return err
}

func (d *driver) setupCgroups(container *libcontainer.Config, c *execdriver.Command) error {
	if c.Resources != nil {
		container.Cgroups.CpuShares = c.Resources.CpuShares
		container.Cgroups.Memory = c.Resources.Memory
		container.Cgroups.MemoryReservation = c.Resources.Memory
		container.Cgroups.MemorySwap = c.Resources.MemorySwap
		container.Cgroups.CpusetCpus = c.Resources.Cpuset
	}

	return nil
}

func (d *driver) setupMounts(container *libcontainer.Config, c *execdriver.Command) error {
	for _, m := range c.Mounts {
		container.MountConfig.Mounts = append(container.MountConfig.Mounts, mount.Mount{
			Type:        "bind",
			Source:      m.Source,
			Destination: m.Destination,
			Writable:    m.Writable,
			Private:     m.Private,
		})
	}

	return nil
}

func (d *driver) setupLabels(container *libcontainer.Config, c *execdriver.Command) error {
	container.ProcessLabel = c.Config["process_label"][0]
	container.MountConfig.MountLabel = c.Config["mount_label"][0]

	return nil
}
