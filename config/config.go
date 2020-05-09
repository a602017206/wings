package config

import (
	"errors"
	"fmt"
	"github.com/cobaugh/osrelease"
	"github.com/creasty/defaults"
	"github.com/gbrlsnchs/jwt/v3"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	"io/ioutil"
	"os"
	"os/exec"
	"os/user"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const DefaultLocation = "/etc/pterodactyl/config.yml"

type Configuration struct {
	sync.RWMutex `json:"-" yaml:"-"`

	// The location from which this configuration instance was instantiated.
	path string

	// Locker specific to writing the configuration to the disk, this happens
	// in areas that might already be locked so we don't want to crash the process.
	writeLock sync.Mutex

	// Determines if wings should be running in debug mode. This value is ignored
	// if the debug flag is passed through the command line arguments.
	Debug bool

	// A unique identifier for this node in the Panel.
	Uuid string

	// An identifier for the token which must be included in any requests to the panel
	// so that the token can be looked up correctly.
	AuthenticationTokenId string `json:"token_id" yaml:"token_id"`

	// The token used when performing operations. Requests to this instance must
	// validate against it.
	AuthenticationToken string `json:"token" yaml:"token"`

	Api    ApiConfiguration
	System SystemConfiguration
	Docker DockerConfiguration

	// The amount of time in seconds that should elapse between disk usage checks
	// run by the daemon. Setting a higher number can result in better IO performance
	// at an increased risk of a malicious user creating a process that goes over
	// the assigned disk limits.
	DiskCheckTimeout int `yaml:"disk_check_timeout"`

	// Defines internal throttling configurations for server processes to prevent
	// someone from running an endless loop that spams data to logs.
	Throttles struct {
		// The number of data overage warnings (inclusive) that can accumulate
		// before a process is terminated.
		KillAtCount int `default:"5" yaml:"kill_at_count"`

		// The number of seconds that must elapse before the internal counter
		// begins decrementing warnings assigned to a process that is outputting
		// too much data.
		DecaySeconds int `default:"10" json:"decay" yaml:"decay"`

		// The total number of bytes allowed to be output by a server process
		// per interval.
		BytesPerInterval int `default:"4096" json:"bytes" yaml:"bytes"`

		// The amount of time that should lapse between data output throttle
		// checks. This should be defined in milliseconds.
		CheckInterval int `default:"100" yaml:"check_interval"`
	}

	// The location where the panel is running that this daemon should connect to
	// to collect data and send events.
	PanelLocation string `json:"remote" yaml:"remote"`
}

// Defines the configuration of the internal SFTP server.
type SftpConfiguration struct {
	// If set to false, the internal SFTP server will not be booted and you will need
	// to run the SFTP server independent of this program.
	UseInternalSystem bool `default:"true" json:"use_internal" yaml:"use_internal"`
	// If set to true disk checking will not be performed. This will prevent the SFTP
	// server from checking the total size of a directory when uploading files.
	DisableDiskChecking bool `default:"false" yaml:"disable_disk_checking"`
	// The bind address of the SFTP server.
	Address string `default:"0.0.0.0" json:"bind_address" yaml:"bind_address"`
	// The bind port of the SFTP server.
	Port int `default:"2022" json:"bind_port" yaml:"bind_port"`
	// If set to true, no write actions will be allowed on the SFTP server.
	ReadOnly bool `default:"false" yaml:"read_only"`
}

// Defines the configuration for the internal API that is exposed by the
// daemon webserver.
type ApiConfiguration struct {
	// The interface that the internal webserver should bind to.
	Host string `default:"0.0.0.0" yaml:"host"`

	// The port that the internal webserver should bind to.
	Port int `default:"8080" yaml:"port"`

	// SSL configuration for the daemon.
	Ssl struct {
		Enabled         bool   `default:"false"`
		CertificateFile string `json:"cert" yaml:"cert"`
		KeyFile         string `json:"key" yaml:"key"`
	}

	// The maximum size for files uploaded through the Panel in bytes.
	UploadLimit int `default:"100" json:"upload_limit" yaml:"upload_limit"`
}

// Reads the configuration from the provided file and returns the configuration
// object that can then be used.
func ReadConfiguration(path string) (*Configuration, error) {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}

	c := new(Configuration)
	// Configures the default values for many of the configuration options present
	// in the structs. Values set in the configuration file take priority over the
	// default values.
	if err := defaults.Set(c); err != nil {
		return nil, err
	}

	// Track the location where we created this configuration.
	c.path = path

	// Replace environment variables within the configuration file with their
	// values from the host system.
	b = []byte(os.ExpandEnv(string(b)))

	if err := yaml.Unmarshal(b, c); err != nil {
		return nil, err
	}

	return c, nil
}

var Mutex sync.RWMutex

var _config *Configuration
var _jwtAlgo *jwt.HMACSHA
var _debugViaFlag bool

// Set the global configuration instance. This is a blocking operation such that
// anything trying to set a different configuration value, or read the configuration
// will be paused until it is complete.
func Set(c *Configuration) {
	Mutex.Lock()

	if _config == nil || _config.AuthenticationToken != c.AuthenticationToken {
		_jwtAlgo = jwt.NewHS256([]byte(c.AuthenticationToken))
	}

	_config = c
	Mutex.Unlock()
}

func SetDebugViaFlag(d bool) {
	_debugViaFlag = d
}

// Get the global configuration instance. This is a read-safe operation that will block
// if the configuration is presently being modified.
func Get() *Configuration {
	Mutex.RLock()
	defer Mutex.RUnlock()

	return _config
}

// Returns the in-memory JWT algorithm.
func GetJwtAlgorithm() *jwt.HMACSHA {
	Mutex.RLock()
	defer Mutex.RUnlock()

	return _jwtAlgo
}

// Returns the path for this configuration file.
func (c *Configuration) GetPath() string {
	return c.path
}

// Ensures that the Pterodactyl core user exists on the system. This user will be the
// owner of all data in the root data directory and is used as the user within containers.
//
// If files are not owned by this user there will be issues with permissions on Docker
// mount points.
func (c *Configuration) EnsurePterodactylUser() (*user.User, error) {
	u, err := user.Lookup(c.System.Username)

	// If an error is returned but it isn't the unknown user error just abort
	// the process entirely. If we did find a user, return it immediately.
	if err == nil {
		return u, c.setSystemUser(u)
	} else if _, ok := err.(user.UnknownUserError); !ok {
		return nil, err
	}

	sysName, err := getSystemName()
	if err != nil {
		return nil, err
	}

	var command = fmt.Sprintf("useradd --system --no-create-home --shell /bin/false %s", c.System.Username)

	// Alpine Linux is the only OS we currently support that doesn't work with the useradd command, so
	// in those cases we just modify the command a bit to work as expected.
	if strings.HasPrefix(sysName, "alpine") {
		command = fmt.Sprintf("adduser -S -D -H -G %[1]s -s /bin/false %[1]s", c.System.Username)

		// We have to create the group first on Alpine, so do that here before continuing on
		// to the user creation process.
		if _, err := exec.Command("addgroup", "-S", c.System.Username).Output(); err != nil {
			return nil, err
		}
	}

	split := strings.Split(command, " ")
	if _, err := exec.Command(split[0], split[1:]...).Output(); err != nil {
		return nil, err
	}

	if u, err := user.Lookup(c.System.Username); err != nil {
		return nil, err
	} else {
		return u, c.setSystemUser(u)
	}
}

// Set the system user into the configuration and then write it to the disk so that
// it is persisted on boot.
func (c *Configuration) setSystemUser(u *user.User) error {
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	c.Lock()
	defer c.Unlock()

	c.System.Username = u.Username
	c.System.User.Uid = uid
	c.System.User.Gid = gid

	return c.WriteToDisk()
}

// Ensures that the configured data directory has the correct permissions assigned to
// all of the files and folders within.
func (c *Configuration) EnsureFilePermissions() error {
	// Don't run this unless it is configured to be run. On large system this can often slow
	// things down dramatically during the boot process.
	if !c.System.SetPermissionsOnBoot {
		return nil
	}

	r := regexp.MustCompile("^[a-f0-9]{8}-[a-f0-9]{4}-4[a-f0-9]{3}-[89ab][a-f0-9]{3}-[a-f0-9]{12}$")

	files, err := ioutil.ReadDir(c.System.Data)
	if err != nil {
		return err
	}

	su, err := user.Lookup(c.System.Username)
	if err != nil {
		return err
	}

	wg := new(sync.WaitGroup)

	for _, file := range files {
		wg.Add(1)

		// Asynchronously run through the list of files and folders in the data directory. If
		// the item is not a folder, or is not a folder that matches the expected UUIDv4 format
		// skip over it.
		//
		// If we do have a positive match, run a chown against the directory.
		go func(f os.FileInfo) {
			defer wg.Done()

			if !f.IsDir() || !r.MatchString(f.Name()) {
				return
			}

			uid, _ := strconv.Atoi(su.Uid)
			gid, _ := strconv.Atoi(su.Gid)

			if err := os.Chown(path.Join(c.System.Data, f.Name()), uid, gid); err != nil {
				zap.S().Warnw("failed to chown server directory", zap.String("directory", f.Name()), zap.Error(err))
			}
		}(file)
	}

	wg.Wait()

	return nil
}

// Writes the configuration to the disk as a blocking operation by obtaining an exclusive
// lock on the file. This prevents something else from writing at the exact same time and
// leading to bad data conditions.
func (c *Configuration) WriteToDisk() error {
	ccopy := *c
	// If debugging is set with the flag, don't save that to the configuration file, otherwise
	// you'll always end up in debug mode.
	if _debugViaFlag {
		ccopy.Debug = false
	}

	if c.path == "" {
		return errors.New("cannot write configuration, no path defined in struct")
	}

	b, err := yaml.Marshal(&ccopy)
	if err != nil {
		return err
	}

	// Obtain an exclusive write against the configuration file.
	c.writeLock.Lock()
	defer c.writeLock.Unlock()

	if err := ioutil.WriteFile(c.GetPath(), b, 0644); err != nil {
		return err
	}

	return nil
}

// Gets the system release name.
func getSystemName() (string, error) {
	// use osrelease to get release version and ID
	if release, err := osrelease.Read(); err != nil {
		return "", err
	} else {
		return release["ID"], nil
	}
}
