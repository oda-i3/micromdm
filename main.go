package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"

	"github.com/go-kit/kit/log"
	"github.com/micromdm/micromdm/checkin"
	"github.com/micromdm/micromdm/command"
	"github.com/micromdm/micromdm/connect"
	"github.com/micromdm/micromdm/device"
	"github.com/micromdm/micromdm/profile"
	"github.com/micromdm/micromdm/workflow"
	stdprometheus "github.com/prometheus/client_golang/prometheus"
	"golang.org/x/net/context"
)

var (
	// Version info
	Version = "unreleased"
	gitHash = "unknown"
)

func main() {
	ctx := context.Background()
	logger := log.NewLogfmtLogger(os.Stderr)

	//flags
	var (
		flPort       = flag.String("port", envString("MICROMDM_HTTP_LISTEN_PORT", ""), "port to listen on")
		flTLS        = flag.Bool("tls", envBool("MICROMDM_USE_TLS"), "use https")
		flTLSCert    = flag.String("tls-cert", envString("MICROMDM_TLS_CERT", ""), "path to TLS certificate")
		flTLSKey     = flag.String("tls-key", envString("MICROMDM_TLS_KEY", ""), "path to TLS private key")
		flPGconn     = flag.String("postgres", envString("MICROMDM_POSTGRES_CONN_URL", ""), "postgres connection url")
		flRedisconn  = flag.String("redis", envString("MICROMDM_REDIS_CONN_URL", ""), "redis connection url")
		flVersion    = flag.Bool("version", false, "print version information")
		flPushCert   = flag.String("push-cert", envString("MICROMDM_PUSH_CERT", ""), "path to push certificate")
		flPushPass   = flag.String("push-pass", envString("MICROMDM_PUSH_PASS", ""), "push certificate password")
		flEnrollment = flag.String("profile", envString("MICROMDM_ENROLL_PROFILE", ""), "path to enrollment profile")
	)

	// set tls to true by default. let user set it to false
	*flTLS = true
	flag.Parse()

	// -version flag
	if *flVersion {
		fmt.Printf("micromdm - Version %s\n", Version)
		fmt.Printf("Git Hash - %s\n", gitHash)
		os.Exit(0)
	}

	// check port flag
	// if none is provided, default to 80 or 443
	if *flPort == "" {
		port := defaultPort(*flTLS)
		logger.Log("msg", fmt.Sprintf("No port flag specified. Using %v by default", port))
		*flPort = port
	}

	if *flEnrollment == "" {
		logger.Log("err", "must set path to enrollment profile")
		os.Exit(1)
	}

	// check cert and key if -tls=true
	if *flTLS {
		if err := checkTLSFlags(*flTLSKey, *flTLSCert); err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}
	}

	pgHostAddr := os.Getenv("POSTGRES_PORT_5432_TCP_ADDR")
	if *flPGconn == "" && pgHostAddr != "" {
		*flPGconn = getPGConnFromENV(logger, pgHostAddr)
	}

	// check database connection
	if *flPGconn == "" {
		logger.Log("err", "database connection url not specified")
		os.Exit(1)
	}

	//profileDB must exist before workflowDB
	// TODO this is getting messy and migrations should be handled by a separate thing
	profileDB := profile.NewDB(
		"postgres",
		*flPGconn,
		profile.Logger(logger),
		profile.Debug(),
	)

	workflowDB := workflow.NewDB(
		"postgres",
		*flPGconn,
		workflow.Logger(logger),
		workflow.Debug(),
	)

	deviceDB := device.NewDB(
		"postgres",
		*flPGconn,
		device.Logger(logger),
		device.EnrollProfile(*flEnrollment),
	)

	// Checkin Service
	checkinSvc := checkin.NewCheckinService(
		checkin.Datastore(deviceDB),
		checkin.Logger(logger),
		checkin.Push(*flPushCert, *flPushPass),
	)
	checkinHandler := checkin.ServiceHandler(ctx, checkinSvc)

	redisHostAddr := os.Getenv("REDIS_PORT_6379_TCP_ADDR")
	if *flRedisconn == "" && redisHostAddr != "" {
		*flRedisconn = getRedisConnFromENV(redisHostAddr)
	}

	// check database connection
	if *flRedisconn == "" {
		logger.Log("err", "database connection url not specified")
		os.Exit(1)
	}

	commandDB := command.NewDB(
		"redis",
		*flRedisconn,
		command.Logger(logger),
	)

	commandSvc := command.NewCommandService(
		command.DB(commandDB),
		command.Logger(logger),
	)
	commandHandler := command.ServiceHandler(ctx, commandSvc)

	connectSvc := connect.NewConnectService(
		connect.Redis(commandDB),
	)
	connectHandler := connect.ServiceHandler(ctx, connectSvc)

	workflowSvc := workflow.NewService(workflow.DB(workflowDB))
	workflowHandler := workflow.ServiceHandler(ctx, workflowSvc)
	// profiles

	profileSvc := profile.NewService(profile.DB(profileDB))
	profileHandler := profile.ServiceHandler(ctx, profileSvc)

	deviceSvc := device.NewService(deviceDB)
	deviceHandler := device.ServiceHandler(ctx, deviceSvc)

	mux := http.NewServeMux()
	mux.Handle("/mdm/checkin", checkinHandler)
	mux.Handle("/mdm/command", commandHandler)
	mux.Handle("/mdm/connect", connectHandler)
	mux.Handle("/mdm/workflows", workflowHandler)
	mux.Handle("/mdm/profiles", profileHandler)
	mux.Handle("/mdm/devices/", deviceHandler)

	http.Handle("/", mux)
	http.Handle("/metrics", stdprometheus.Handler())

	serve(logger, *flTLS, *flPort, *flTLSKey, *flTLSCert)
}

// choose http or https
func serve(logger log.Logger, tls bool, port, key, cert string) {
	portStr := fmt.Sprintf(":%v", port)
	if tls {
		logger.Log("msg", "HTTPs", "addr", port)
		logger.Log("err", http.ListenAndServeTLS(portStr, cert, key, nil))
	} else {
		logger.Log("msg", "HTTP", "addr", port)
		logger.Log("err", http.ListenAndServe(portStr, nil))
	}
}

func envString(key, def string) string {
	if env := os.Getenv(key); env != "" {
		return env
	}
	return def
}

func envBool(key string) bool {
	if env := os.Getenv(key); env == "true" {
		return true
	}
	return false
}

func checkTLSFlags(key, cert string) error {
	if key == "" || cert == "" {
		return errors.New("You must provide a valid path to a TLS cert and key")
	}
	return nil
}

func defaultPort(tls bool) string {
	if tls {
		return "443"
	}
	return "80"
}

// use this in docker container
func getPGConnFromENV(logger log.Logger, host string) string {
	user := os.Getenv("POSTGRES_ENV_POSTGRES_USER")
	if user == "" {
		user = "postgres"
	}
	dbname := os.Getenv("POSTGRES_ENV_POSTGRES_DB")
	if dbname == "" {
		dbname = user //same defaults as the docker pgcontainer
	}
	password := os.Getenv("POSTGRES_ENV_POSTGRES_PASSWORD")
	if password == "" {
		password = "postgres"
	}
	sslmode := os.Getenv("POSTGRES_ENV_SSLMODE")
	if sslmode == "" {
		logger.Log("msg", "POSTGRES_ENV_SSLMODE not specified, using 'require' by default")
		sslmode = "require"
	}
	conn := fmt.Sprintf("user=%v password=%v dbname=%v sslmode=%v host=%v", user, password, dbname, sslmode, host)
	return conn
}

func getRedisConnFromENV(host string) string {
	port := os.Getenv("REDIS_PORT_6379_TCP_PORT")
	conn := fmt.Sprintf("%v:%v", host, port)
	return conn
}
