package spread

import (
	"encoding/json"
	"fmt"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

func Linode(b *Backend) Provider {
	return &linode{backend: b}
}

type linode struct {
	backend *Backend

	distrosLock  sync.Mutex
	distrosDone  bool
	distrosCache []*linodeDistro
	kernelsCache []*linodeKernel
}

var client = &http.Client{}

type linodeServer struct {
	l *linode

	ID     int     `json:"LINODEID"`
	Label  string  `json:"LABEL"`
	Status int     `json:"STATUS" yaml:"-"`
	Addr   string  `json:"-" yaml:"address"`
	Img    ImageID `json:"-" yaml:"image"`
	Config int     `json:"-"`
	Root   int     `json:"-"`
	Swap   int     `json:"-"`
}

func (s *linodeServer) String() string {
	return fmt.Sprintf("%s:%s (%s)", s.l.backend.Name, s.Img.SystemID(), s.Label)
}

func (s *linodeServer) Provider() Provider {
	return s.l
}

func (s *linodeServer) Address() string {
	return s.Addr
}

func (s *linodeServer) Image() ImageID {
	return s.Img
}

func (s *linodeServer) Snapshot() (ImageID, error) {
	return "", nil
}

func (s *linodeServer) ReuseData() []byte {
	data, err := yaml.Marshal(s)
	if err != nil {
		panic(err)
	}
	return data
}

const (
	linodeBeingCreated = -1
	linodeBrandNew     = 0
	linodeRunning      = 1
	linodePoweredOff   = 2
)

type linodeResult struct {
	Errors []linodeError `json:"ERRORARRAY"`
}

type linodeError struct {
	Code    int    `json:"ERRORCODE"`
	Message string `json:"ERRORMESSAGE"`
}

func (r *linodeResult) err() error {
	for _, e := range r.Errors {
		return fmt.Errorf("%s", strings.ToLower(string(e.Message[0]))+e.Message[1:])
	}
	return nil
}

func (l *linode) Backend() *Backend {
	return l.backend
}

func (l *linode) DiscardSnapshot(image ImageID) error {
	return nil
}

func (l *linode) Reuse(data []byte, password string) (Server, error) {
	server := &linodeServer{}
	err := yaml.Unmarshal(data, server)
	if err != nil {
		return nil, fmt.Errorf("cannot unmarshal Linode reuse data: %v", err)
	}
	server.l = l
	return server, nil
}

type FatalError struct {
	error
}

func (l *linode) Allocate(image ImageID, password string) (Server, error) {
	servers, err := l.list()
	if err != nil {
		return nil, err
	}
	if len(servers) == 0 {
		return nil, FatalError{fmt.Errorf("no servers in Linode account")}
	}
	for _, server := range servers {
		if server.Status != linodePoweredOff {
			continue
		}
		err := l.setup(server, image, password)
		if err != nil {
			return nil, err
		}
		printf("Allocated %s.", server)
		return server, nil
	}
	return nil, fmt.Errorf("no powered off servers in Linode account")
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *linodeServer) Discard() error {
	logf("Discarding %s...", s)
	_, err1 := s.l.shutdown(s)
	err2 := s.l.removeConfig(s, s.Config)
	err3 := s.l.removeDisks(s, s.Root, s.Swap)
	return firstErr(err1, err2, err3)
}

type linodeListResult struct {
	Data []*linodeServer `json:"DATA"`
}

func (l *linode) list() ([]*linodeServer, error) {
	log("Listing available Linode servers...")
	params := linodeParams{
		"api_action": "linode.list",
	}
	var result linodeListResult
	err := l.do(params, &result)
	if err != nil {
		return nil, err
	}
	return result.Data, nil
}

func (l *linode) setup(server *linodeServer, image ImageID, password string) error {
	server.l = l
	server.Img = image

	ip, err := l.ip(server)
	if err != nil {
		return err
	}
	server.Addr = ip.IPAddress

	rootJob, swapJob, err := l.createDisk(server, image, password)
	if err != nil {
		return err
	}
	server.Root = rootJob.DiskID
	server.Swap = swapJob.DiskID

	configID, err := l.createConfig(server, image, server.Root, server.Swap)
	if err != nil {
		l.removeDisks(server, server.Root, server.Swap)
		return err
	}
	server.Config = configID

	bootJob, err := l.boot(server, configID)
	if err == nil {
		_, err = l.waitJob(server, "boot", bootJob.JobID)
	}
	if err != nil {
		l.removeConfig(server, server.Config)
		l.removeDisks(server, server.Root, server.Swap)
		return err
	}
	return nil
}

type linodeJob struct {
	JobID int `json:"JOBID"`
}

type linodeJobResult struct {
	linodeResult
	Data *linodeJob `json:"DATA"`
}

func (l *linode) boot(server *linodeServer, configID int) (*linodeJob, error) {
	return l.serverJob(server, "reboot", linodeParams{
		"api_action": "linode.reboot",
		"LinodeID":   server.ID,
		"ConfigID":   configID,
	})
}

func (l *linode) reboot(server *linodeServer, configID int) (*linodeJob, error) {
	return l.serverJob(server, "reboot", linodeParams{
		"api_action": "linode.reboot",
		"LinodeID":   server.ID,
		"ConfigID":   configID,
	})
}

func (l *linode) shutdown(server *linodeServer) (*linodeJob, error) {
	return l.serverJob(server, "shutdown", linodeParams{
		"api_action": "linode.shutdown",
		"LinodeID":   server.ID,
	})
}

func (l *linode) serverJob(server *linodeServer, verb string, params linodeParams) (*linodeJob, error) {
	var result linodeJobResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return nil, fmt.Errorf("cannot %s %s: %v", verb, server, err)
	}
	return result.Data, nil
}

type linodeDiskJob struct {
	DiskID int `json:"DISKID"`
	JobID  int `json:"JOBID"`
}

type linodeDiskJobResult struct {
	linodeResult
	Data *linodeDiskJob `json:"DATA"`
}

func (l *linode) createDisk(server *linodeServer, image ImageID, password string) (root, swap *linodeDiskJob, err error) {
	distro, err := l.distro(image)
	if err != nil {
		return nil, nil, err
	}

	logf("Creating disk on %s with %s...", server, image)
	params := linodeParams{
		"api_action": "batch",
		"api_requestArray": []linodeParams{{
			"api_action":     "linode.disk.createFromDistribution",
			"LinodeID":       server.ID,
			"DistributionID": distro.ID,
			"Label":          image.Label("root"),
			"Size":           4096,
			"rootPass":       password,
		}, {
			"api_action": "linode.disk.create",
			"LinodeID":   server.ID,
			"Label":      image.Label("swap"),
			"Size":       256,
			"Type":       "swap",
		}},
	}

	var results []linodeDiskJobResult
	err = l.do(params, &results)
	for i, result := range results {
		if e := result.err(); e != nil {
			err = e
			break
		}
		if i == 0 {
			root = result.Data
			continue
		}
		swap = result.Data
		return root, swap, nil
	}

	if root != nil {
		l.removeDisks(server, root.DiskID)
	}
	if len(results) == 0 {
		err = fmt.Errorf("empty batch result")
	}
	return nil, nil, fmt.Errorf("cannot create Linode disk with %s: %v", image, err)
}

func (l *linode) removeDisks(server *linodeServer, diskIDs ...int) error {
	logf("Removing disks from %s...", server)
	var batch []linodeParams
	for _, diskID := range diskIDs {
		batch = append(batch, linodeParams{
			"api_action": "linode.disk.delete",
			"LinodeID":   server.ID,
			"DiskID":     diskID,
		})
	}
	params := linodeParams{
		"api_action":       "batch",
		"api_requestArray": batch,
	}
	var results []linodeResult
	err := l.do(params, &results)
	if err != nil {
		return fmt.Errorf("cannot remove disk on %s: %v", server, err)
	}
	for _, result := range results {
		if err := result.err(); err != nil {
			return fmt.Errorf("cannot remove disk on %s: %v", server, err)
		}
	}
	return nil
}

type linodeConfigResult struct {
	linodeResult
	Data struct {
		ConfigID int `json:"CONFIGID"`
	} `json:"DATA"`
}

func (l *linode) createConfig(server *linodeServer, image ImageID, rootID, swapID int) (configID int, err error) {
	logf("Creating configuration on %s with %s...", server, image)

	distro, err := l.distro(image)
	if err != nil {
		return 0, err
	}

	params := linodeParams{
		"api_action":             "linode.config.create",
		"LinodeID":               server.ID,
		"KernelID":               distro.KernelID,
		"Label":                  image.Label(""),
		"DiskList":               fmt.Sprintf("%d,%d", rootID, swapID),
		"RootDeviceNum":          1,
		"RootDeviceR0":           true,
		"helper_disableUpdateDB": true,
		"helper_distro":          true,
		"helper_depmod":          true,
		"helper_network":         false,
		"devtmpfs_automount":     true,
	}

	var result linodeConfigResult
	err = l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return 0, fmt.Errorf("cannot create config on %s with %s: %v", server, image, err)
	}
	return result.Data.ConfigID, nil
}

func (l *linode) removeConfig(server *linodeServer, configID int) error {
	logf("Removing configuration from %s...", server)

	params := linodeParams{
		"api_action": "linode.config.delete",
		"LinodeID":   server.ID,
		"ConfigID":   configID,
	}
	var result linodeResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err != nil {
		return fmt.Errorf("cannot remove config from %s: %v", server, err)
	}
	return nil
}

type linodeJobInfo struct {
	JobID       int    `json:"JOBID"`
	LinodeID    int    `json:"LINODEID"`
	Action      string `json:"ACTION"`
	Label       string `json:"LABEL"`
	HostStart   string `json:"HOST_START_DT"`
	HostFinish  string `json:"HOST_FINISH_DT"`
	HostSuccess int    `json:"HOST_SUCCESS"`
	HostMessage string `json:"HOST_MESSAGE"`
}

func (job *linodeJobInfo) err() error {
	if job.HostSuccess == 1 || job.HostFinish == "" {
		return nil
	}
	if msg := job.HostMessage; msg != "" {
		return fmt.Errorf("%s", strings.ToLower(string(msg[0]))+msg[1:])
	}
	return fmt.Errorf("job %d failed silently", job.JobID)
}

type linodeJobInfoResult struct {
	linodeResult
	Data []*linodeJobInfo `json:"DATA"`
}

func (l *linode) jobInfo(server *linodeServer, jobID int) (*linodeJobInfo, error) {
	params := linodeParams{
		"api_action": "linode.job.list",
		"LinodeID":   server.ID,
		"JobID":      jobID,
	}
	var result linodeJobInfoResult
	err := l.do(params, &result)
	if err == nil {
		err = result.err()
	}
	if err == nil && len(result.Data) == 0 {
		err = fmt.Errorf("empty result")
	}
	if err != nil {
		return nil, fmt.Errorf("cannot get job details for %s: %v", server, err)
	}
	return result.Data[0], nil
}

func (l *linode) waitJob(server *linodeServer, verb string, jobID int) (*linodeJobInfo, error) {
	logf("Waiting for %s to %s...", server, verb)

	timeout := time.After(1 * time.Minute)
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()

	var infoErr error
	for {
		select {
		case <-timeout:
			// Don't shutdown. The machine may be running something else.
			if infoErr != nil {
				return nil, infoErr
			}
			l.removeConfig(server, server.Config)
			l.removeDisks(server, server.Root, server.Swap)
			return nil, fmt.Errorf("timeout waiting for %s to %s", server, verb)

		case <-retry.C:
			job, err := l.jobInfo(server, jobID)
			if err != nil {
				infoErr = fmt.Errorf("cannot %s %s: %s", verb, server, err)
				break
			}
			if job.HostFinish != "" {
				err := job.err()
				if err != nil {
					err = fmt.Errorf("cannot %s %s: %s", verb, server, err)
				}
				return job, err
			}
		}
	}
	panic("unreachable")
}

type linodeIPResult struct {
	linodeResult
	Data []*linodeIP `json:"DATA"`
}

type linodeIP struct {
	ID        int    `json:"IPADDRESSID"`
	LinodeID  int    `json:"LINODEID"`
	IsPublic  int    `json:"ISPUBLIC"`
	IPAddress string `json:"IPADDRESS"`
	RDNSName  string `json:"RDNS_NAME"`
}

func (l *linode) ip(server *linodeServer) (*linodeIP, error) {
	logf("Obtaining address of %s...", server)

	params := linodeParams{
		"api_action": "linode.ip.list",
		"LinodeID":   server.ID,
	}
	var result linodeIPResult
	err := l.do(params, &result)
	if err != nil {
		return nil, err
	}
	if err := result.err(); err != nil {
		return nil, fmt.Errorf("cannot list IPs for %s: %v", server, err)
	}
	for _, ip := range result.Data {
		if ip.IsPublic == 1 {
			logf("Got address of %s: %s", server, ip.IPAddress)
			return ip, nil
		}
	}
	return nil, fmt.Errorf("cannot find public IP for %s", server)
}

type distrosResult struct {
	linodeResult
	Data []*linodeDistro
}

type linodeDistro struct {
	Name     string `json:"-"`
	KernelID int    `json:"-"`

	ID           int    `json:"DISTRIBUTIONID"`
	Label        string `json:"LABEL"`
	MinImageSize int    `json:"MINIMAGESIZE"`
	VOPSKernel   int    `json:"REQUIRESVOPSKERNEL"`
	Is64Bit      int    `json:"IS64BIT"`
	Create       string `json:"CREATE_DT"`
}

type kernelsResult struct {
	linodeResult
	Data []*linodeKernel
}

type linodeKernel struct {
	ID      int    `json:"KERNELID"`
	IsPVOPS int    `json:"ISPVOPS"`
	IsXEN   int    `json:"ISXEN"`
	IsKVM   int    `json:"ISKVM"`
	Label   string `json:"LABEL"`
}

func (l *linode) distro(image ImageID) (*linodeDistro, error) {
	l.distrosLock.Lock()
	defer l.distrosLock.Unlock()

	if !l.distrosDone {
		if err := l.cacheDistros(); err != nil {
			return nil, err
		}
	}
	l.distrosDone = true

	var system = string(image.SystemID())
	var best *linodeDistro
	for _, distro := range l.distrosCache {
		if distro.Name != system {
			continue
		}
		if distro.Is64Bit == 1 {
			return distro, nil
		}
		best = distro
	}
	if best == nil {
		return nil, fmt.Errorf("cannot find system %s in Linode")
	}
	return best, nil
}

func (l *linode) cacheDistros() error {
	var err error
	for retry := 0; retry < 3; retry++ {
		params := linodeParams{
			"api_action": "avail.distributions",
		}
		var result distrosResult
		err = l.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			l.distrosCache = result.Data
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot list Linode distributions: %v", err)
	}
	for retry := 0; retry < 3; retry++ {
		params := linodeParams{
			"api_action": "avail.kernels",
		}
		var result kernelsResult
		err = l.do(params, &result)
		if err == nil {
			err = result.err()
		}
		if err == nil {
			l.kernelsCache = result.Data
			break
		}
	}
	if err != nil {
		return fmt.Errorf("cannot list Linode kernels: %v", err)
	}

	var latest32 = -1
	var latest64 = -1
	for _, kernel := range l.kernelsCache {
		if strings.HasPrefix(kernel.Label, "Latest 64 bit") {
			latest64 = kernel.ID
		}
		if strings.HasPrefix(kernel.Label, "Latest 32 bit") {
			latest32 = kernel.ID
		}
	}
	if latest32 == -1 || latest64 == -1 {
		return fmt.Errorf("cannot find latest Linode kernel")
	}
	for _, distro := range l.distrosCache {
		if distro.Is64Bit == 1 {
			distro.KernelID = latest64
		} else {
			distro.KernelID = latest32
		}

		label := strings.Fields(strings.ToLower(distro.Label))
		if len(label) > 2 && label[1] == "linux" {
			distro.Name = label[0] + "-" + label[2]
		} else {
			distro.Name = label[0] + "-" + label[1]
		}
	}

	debugf("Linode distributions available: %# v", l.distrosCache)
	return nil
}

type linodeParams map[string]interface{}

func (l *linode) do(params linodeParams, result interface{}) error {
	debugf("Linode request: %# v\n", params)

	values := make(url.Values)
	for k, v := range params {
		var vs string
		switch v := v.(type) {
		case int:
			vs = strconv.Itoa(v)
		case string:
			vs = v
		default:
			data, err := json.Marshal(v)
			if err != nil {
				return fmt.Errorf("cannot marshal Linode request parameter %q: %s", k, err)
			}
			vs = string(data)
		}
		values[k] = []string{vs}
	}
	values["api_key"] = []string{l.backend.Key}

	resp, err := client.PostForm("https://api.linode.com", values)
	if err != nil {
		return fmt.Errorf("cannot perform Linode request: %v", err)
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("cannot read Linode response: %v", err)
	}

	if Debug {
		var r interface{}
		err = json.Unmarshal(data, &r)
		if err != nil {
			return fmt.Errorf("cannot decode Linode response: %v", err)
		}
		debugf("Linode response: %# v\n", r)
	}

	err = json.Unmarshal(data, result)
	if err != nil {
		return fmt.Errorf("cannot decode Linode response: %v", err)
	}
	return nil
}
