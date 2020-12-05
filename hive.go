package gohive

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"os/user"
	"strconv"
	"strings"
	"sync"
	"time"
	"log"

	"github.com/apache/thrift/lib/go/thrift"
	"github.com/beltran/gohive/hiveserver"
	"github.com/beltran/gosasl"
	"github.com/go-zookeeper/zk"
)

const DEFAULT_FETCH_SIZE int64 = 1000
const ZOOKEEPER_DEFAULT_NAMESPACE = "hiveserver2"

// Connection holds the information for getting a cursor to hive
type Connection struct {
	host                string
	port                int
	username            string
	database            string
	auth                string
	kerberosServiceName string
	password            string
	sessionHandle       *hiveserver.TSessionHandle
	client              *hiveserver.TCLIServiceClient
	configuration       *ConnectConfiguration
	transport           thrift.TTransport
}

// ConnectConfiguration is the configuration for the connection
// The fields have to be filled manually but not all of them are required
// Depends on the auth and kind of connection.
type ConnectConfiguration struct {
	Username             string
	Principal            string
	Password             string
	Service              string
	HiveConfiguration    map[string]string
	PollIntervalInMillis int
	FetchSize            int64
	TransportMode        string
	HTTPPath             string
	TLSConfig            *tls.Config
	ZookeeperNamespace   string
	Database             string
}

// NewConnectConfiguration returns a connect configuration, all with empty fields
func NewConnectConfiguration() *ConnectConfiguration {
	return &ConnectConfiguration{
		Username:             "",
		Password:             "",
		Service:              "",
		HiveConfiguration:    nil,
		PollIntervalInMillis: 200,
		FetchSize:            DEFAULT_FETCH_SIZE,
		TransportMode:        "binary",
		HTTPPath:             "cliservice",
		TLSConfig:            nil,
		ZookeeperNamespace:   ZOOKEEPER_DEFAULT_NAMESPACE,
	}
}

// Connect to zookeper to get hive hosts and then connect to hive.
// hosts is in format host1:port1,host2:port2,host3:port3 (zookeeper hosts).
func ConnectZookeeper(hosts string, auth string,
	configuration *ConnectConfiguration) (conn *Connection, err error) {
	// consider host as zookeeper quorum
	zkHosts := strings.Split(hosts, ",")
	zkConn, _, err := zk.Connect(zkHosts, time.Second)
	if err != nil {
		return nil, err
	}

	hsInfos, _, err := zkConn.Children("/" + configuration.ZookeeperNamespace)
	if err != nil {
		panic(err)
	}
	if len(hsInfos) > 0 {
		nodes := parseHiveServer2Info(hsInfos)
		rand.Shuffle(len(nodes), func(i, j int) {
			nodes[i], nodes[j] = nodes[j], nodes[i]
		})
		for _, node := range nodes {
			port, err := strconv.Atoi(node["port"])
			if err != nil {
				continue
			}
			conn, err := innerConnect(node["host"], port, auth, configuration)
			if err != nil {
				// Let's try to connect to the next one
				continue
			}
			return conn, nil
		}
		return nil, fmt.Errorf("all Hive servers of the specified Zookeeper namespace %s are unavailable",
			configuration.ZookeeperNamespace)
	} else {
		return nil, fmt.Errorf("no Hive server is registered in the specified Zookeeper namespace %s",
			configuration.ZookeeperNamespace)
	}

}

// Connect to hive server
func Connect(host string, port int, auth string,
	configuration *ConnectConfiguration) (conn *Connection, err error) {
	return innerConnect(host, port, auth, configuration)
}

func parseHiveServer2Info(hsInfos []string) []map[string]string {
	results := make([]map[string]string, len(hsInfos))
	actualCount := 0

	for _, hsInfo := range hsInfos {
		validFormat := false
		node := make(map[string]string)

		for _, param := range strings.Split(hsInfo, ";") {
			kvPair := strings.Split(param, "=")
			if len(kvPair) < 2 {
				break
			}
			if kvPair[0] == "serverUri" {
				hostAndPort := strings.Split(kvPair[1], ":")
				if len(hostAndPort) == 2 {
					node["host"] = hostAndPort[0]
					node["port"] = hostAndPort[1]
					validFormat = len(node["host"]) != 0 && len(node["port"]) != 0
				} else {
					break
				}
			} else {
				node[kvPair[0]] = kvPair[1]
			}
		}
		if validFormat {
			results[actualCount] = node
			actualCount++
		}
	}
	return results[0:actualCount]
}

func innerConnect(host string, port int, auth string,
	configuration *ConnectConfiguration) (conn *Connection, err error) {

	var socket thrift.TTransport
	if configuration.TLSConfig != nil {
		socket, err = thrift.NewTSSLSocket(fmt.Sprintf("%s:%d", host, port), configuration.TLSConfig)
	} else {
		socket, err = thrift.NewTSocket(fmt.Sprintf("%s:%d", host, port))
	}

	if err != nil {
		return
	}

	if err = socket.Open(); err != nil {
		return
	}

	var transport thrift.TTransport

	if configuration == nil {
		configuration = NewConnectConfiguration()
	}
	if configuration.Username == "" {
		_user, err := user.Current()
		if err != nil {
			return nil, fmt.Errorf("Can't determine the username")
		}
		configuration.Username = strings.Replace(_user.Name, " ", "", -1)
	}
	// password may not matter but can't be empty
	if configuration.Password == "" {
		configuration.Password = "x"
	}

	if configuration.TransportMode == "http" {
		if auth == "NONE" {
			httpClient, protocol, err := getHTTPClient(configuration)
			if err != nil {
				return nil, err
			}
			httpOptions := thrift.THttpClientOptions{Client: httpClient}
			transport, err = thrift.NewTHttpClientTransportFactoryWithOptions(fmt.Sprintf(protocol+"://%s:%s@%s:%d/"+configuration.HTTPPath, url.QueryEscape(configuration.Username), url.QueryEscape(configuration.Password), host, port), httpOptions).GetTransport(socket)
			if err != nil {
				return nil, err
			}
		} else if auth == "KERBEROS" {
			mechanism, err := gosasl.NewGSSAPIMechanism(configuration.Service)
			if err != nil {
				return nil, err
			}
			saslClient := gosasl.NewSaslClient(host, mechanism)
			token, err := saslClient.Start()
			if err != nil {
				return nil, err
			}
			if len(token) == 0 {
				return nil, fmt.Errorf("Gssapi init context returned an empty token. Probably the service is empty in the configuration")
			}

			httpClient, protocol, err := getHTTPClient(configuration)
			if err != nil {
				return nil, err
			}
			httpClient.Jar = newCookieJar()

			httpOptions := thrift.THttpClientOptions{
				Client: httpClient,
			}
			transport, err = thrift.NewTHttpClientTransportFactoryWithOptions(fmt.Sprintf(protocol+"://%s:%d/"+configuration.HTTPPath, host, port), httpOptions).GetTransport(socket)
			httpTransport, ok := transport.(*thrift.THttpClient)
			if ok {
				httpTransport.SetHeader("Authorization", "Negotiate "+base64.StdEncoding.EncodeToString(token))
			}
			if err != nil {
				return nil, err
			}
		} else {
			panic("Unrecognized auth")
		}
	} else if configuration.TransportMode == "binary" {
		if auth == "NOSASL" {
			transport = thrift.NewTBufferedTransport(socket, 4096)
			if transport == nil {
				return nil, fmt.Errorf("BufferedTransport was nil")
			}
		} else if auth == "NONE" || auth == "LDAP" || auth == "CUSTOM" {
			saslConfiguration := map[string]string{"username": configuration.Username, "password": configuration.Password}
			transport, err = NewTSaslTransport(socket, host, "PLAIN", saslConfiguration)
			if err != nil {
				return
			}
		} else if auth == "KERBEROS" {
			saslConfiguration := map[string]string{"service": configuration.Service}
			transport, err = NewTSaslTransport(socket, host, "GSSAPI", saslConfiguration)
			if err != nil {
				return
			}
		} else if auth == "DIGEST-MD5" {
			saslConfiguration := map[string]string{"username": configuration.Username, "password": configuration.Password, "service": configuration.Service}
			transport, err = NewTSaslTransport(socket, host, "DIGEST-MD5", saslConfiguration)
			if err != nil {
				return
			}
		} else {
			panic("Unrecognized auth")
		}
		if !transport.IsOpen() {
			if err = transport.Open(); err != nil {
				return
			}
		}
	} else {
		panic(fmt.Sprintf("Unrecognized transport mode %s", configuration.TransportMode))
	}

	protocolFactory := thrift.NewTBinaryProtocolFactoryDefault()
	client := hiveserver.NewTCLIServiceClientFactory(transport, protocolFactory)

	openSession := hiveserver.NewTOpenSessionReq()
	openSession.ClientProtocol = hiveserver.TProtocolVersion_HIVE_CLI_SERVICE_PROTOCOL_V3
	openSession.Configuration = configuration.HiveConfiguration
	openSession.Username = &configuration.Username
	openSession.Password = &configuration.Password
	// Context is ignored
	response, err := client.OpenSession(context.Background(), openSession)
	if err != nil {
		return
	}

	database := configuration.Database
	if database == "" {
		database = "default"
	}
	connection := &Connection{
		host:                host,
		port:                port,
		database:            database,
		auth:                auth,
		kerberosServiceName: "",
		sessionHandle:       response.SessionHandle,
		client:              client,
		configuration:       configuration,
		transport:           transport,
	}

	if configuration.Database != "" {
		cursor := connection.Cursor()
		cursor.Exec(context.Background(), "USE "+configuration.Database)
		if cursor.Err != nil {
			return nil, cursor.Err
		}
	}

	return connection, nil
}

func getHTTPClient(configuration *ConnectConfiguration) (httpClient *http.Client, protocol string, err error) {
	if configuration.TLSConfig != nil {
		transport := &http.Transport{TLSClientConfig: configuration.TLSConfig}
		httpClient = &http.Client{Transport: transport}
		protocol = "https"
	} else {
		httpClient = http.DefaultClient
		protocol = "http"
	}
	return
}

// Cursor creates a cursor from a connection
func (c *Connection) Cursor() *Cursor {
	return &Cursor{
		conn:  c,
		queue: make([]*hiveserver.TColumn, 0),
	}
}

// Close closes a session
func (c *Connection) Close() error {
	closeRequest := hiveserver.NewTCloseSessionReq()
	closeRequest.SessionHandle = c.sessionHandle
	// This context is ignored
	responseClose, err := c.client.CloseSession(context.Background(), closeRequest)

	if c.transport != nil {
		errTransport := c.transport.Close()
		if errTransport != nil {
			return errTransport
		}
	}
	if err != nil {
		return err
	}
	if !success(responseClose.GetStatus()) {
		return fmt.Errorf("Error closing the session: %s", responseClose.Status.String())
	}
	return nil
}

const _RUNNING = 0
const _FINISHED = 1
const _NONE = 2
const _CONTEXT_DONE = 3
const _ERROR = 4
const _ASYNC_ENDED = 5

// Cursor is used for fetching the rows after a query
type Cursor struct {
	conn            *Connection
	operationHandle *hiveserver.TOperationHandle
	queue           []*hiveserver.TColumn
	response        *hiveserver.TFetchResultsResp
	columnIndex     int
	totalRows       int
	state           int
	newData         bool
	Err             error
	description     [][]string
}

// WaitForCompletion waits for an async operation to finish
func (c *Cursor) WaitForCompletion(ctx context.Context) {
	done := make(chan interface{}, 1)
	defer close(done)

	var mux sync.Mutex
	var contextDone bool = false

	go func() {
		select {
		case <-done:
		case <-ctx.Done():
			mux.Lock()
			contextDone = true
			mux.Unlock()
		}
	}()

	for true {
		operationStatus := c.Poll(true)
		if c.Err != nil {
			return
		}
		status := operationStatus.OperationState
		finished := !(*status == hiveserver.TOperationState_INITIALIZED_STATE || *status == hiveserver.TOperationState_RUNNING_STATE)
		if finished {
			if *operationStatus.OperationState != hiveserver.TOperationState_FINISHED_STATE {
				msg := operationStatus.Status
				if msg == nil {
					msg = operationStatus.Status
				}
				if s := operationStatus.Status; msg == nil && s != nil {
					msg = s
				}
				if msg == nil {
					*msg.ErrorMessage = fmt.Sprintf("gohive: operation in state without task status or error message")
				}
				c.Err = fmt.Errorf(*msg.ErrorMessage)
			}
			break
		}

		if c.Error() != nil {
			return
		}
		time.Sleep(time.Duration(time.Duration(c.conn.configuration.PollIntervalInMillis)) * time.Millisecond)
		mux.Lock()
		if contextDone {
			c.Err = fmt.Errorf("Context was done before the query was executed")
			c.state = _CONTEXT_DONE
			mux.Unlock()
			return
		}
		mux.Unlock()
	}
	done <- nil
}

// Exec issues a synchronous query.
func (c *Cursor) Exec(ctx context.Context, query string) {
	c.Execute(ctx, query, false)
}

// Execute sends a query to hive for execution with a context
func (c *Cursor) Execute(ctx context.Context, query string, async bool) {
	c.executeAsync(ctx, query)
	if !async {
		// We cannot trust in setting executeReq.RunAsync = true
		// because if the context ends the operation can't be cancelled cleanly
		if c.Err != nil {
			if c.state == _CONTEXT_DONE {
				c.handleDoneContext()
			}
			return
		}
		c.WaitForCompletion(ctx)
		if c.Err != nil {
			if c.state == _CONTEXT_DONE {
				c.handleDoneContext()
			} else if c.state == _ERROR {
				c.Err = fmt.Errorf("Probably the context was over when passed to execute. This probably resulted in the message being sent but we didn't get an operation handle so it's most likely a bug in thrift")
			}
			return
		}
		c.state = _ASYNC_ENDED
	}
}

func (c *Cursor) handleDoneContext() {
	originalError := c.Err
	if c.operationHandle != nil {
		c.Cancel()
		if c.Err != nil {
			return
		}
	}
	c.resetState()
	c.Err = originalError
	c.state = _FINISHED
}

func (c *Cursor) executeAsync(ctx context.Context, query string) {
	c.resetState()

	c.state = _RUNNING
	executeReq := hiveserver.NewTExecuteStatementReq()
	executeReq.SessionHandle = c.conn.sessionHandle
	executeReq.Statement = query
	executeReq.RunAsync = true
	var responseExecute *hiveserver.TExecuteStatementResp

	responseExecute, c.Err = c.conn.client.ExecuteStatement(ctx, executeReq)

	if c.Err != nil {
		if strings.Contains(c.Err.Error(), "context deadline exceeded") {
			c.state = _CONTEXT_DONE
			if responseExecute == nil {
				c.state = _ERROR
			} else {
				// We may need this to cancel the operation
				c.operationHandle = responseExecute.OperationHandle
			}
		}
		return
	}
	if !success(responseExecute.GetStatus()) {
		c.Err = fmt.Errorf("Error while executing query: %s", responseExecute.Status.String())
		return
	}

	c.operationHandle = responseExecute.OperationHandle
	if !responseExecute.OperationHandle.HasResultSet {
		c.state = _FINISHED
	}
}

// Poll returns the current status of the last operation
func (c *Cursor) Poll(getProgres bool) (status *hiveserver.TGetOperationStatusResp) {
	c.Err = nil
	pollRequest := hiveserver.NewTGetOperationStatusReq()
	pollRequest.OperationHandle = c.operationHandle
	var responsePoll *hiveserver.TGetOperationStatusResp
	// Context ignored
	responsePoll, c.Err = c.conn.client.GetOperationStatus(context.Background(), pollRequest)
	if c.Err != nil {
		return nil
	}
	if !success(responsePoll.GetStatus()) {
		c.Err = fmt.Errorf("Error closing the operation: %s", responsePoll.Status.String())
		return nil
	}
	return responsePoll
}

// Finished returns true if the last async operation has finished
func (c *Cursor) Finished() bool {
	operationStatus := c.Poll(true)

	if c.Err != nil {
		return true
	}
	status := operationStatus.OperationState
	return !(*status == hiveserver.TOperationState_INITIALIZED_STATE || *status == hiveserver.TOperationState_RUNNING_STATE)
}

func success(status *hiveserver.TStatus) bool {
	statusCode := status.GetStatusCode()
	return statusCode == hiveserver.TStatusCode_SUCCESS_STATUS || statusCode == hiveserver.TStatusCode_SUCCESS_WITH_INFO_STATUS
}

func (c *Cursor) fetchIfEmpty(ctx context.Context) {
	c.Err = nil
	if c.totalRows == c.columnIndex {
		c.queue = nil
		if !c.HasMore(ctx) {
			c.Err = fmt.Errorf("No more rows are left")
			return
		}
		if c.Err != nil {
			return
		}
	}
}

//RowMap returns one row as a map. Advances the cursor one
func (c *Cursor) RowMap(ctx context.Context) map[string]interface{} {
	c.Err = nil
	c.fetchIfEmpty(ctx)
	if c.Err != nil {
		return nil
	}

	d := c.Description()
	m := make(map[string]interface{}, len(c.queue))
	for i := 0; i < len(c.queue); i++ {
		columnName := d[i][0]
		columnType := d[i][1]
		if columnType == "BOOLEAN_TYPE" {
			if c.queue[i].BoolColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].BoolColumn[c.columnIndex]
			}
		} else if columnType == "TINYINT_TYPE" {
			if c.queue[i].ByteColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].ByteColumn[c.columnIndex].Value
			}
		} else if columnType == "SMALLINT_TYPE" {
			if c.queue[i].I16Column[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].I16Column[c.columnIndex].Value
			}
		} else if columnType == "INT_TYPE" {
			if c.queue[i].I32Column[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].I32Column[c.columnIndex].Value
			}
		} else if columnType == "BIGINT_TYPE" {
			if c.queue[i].I64Column[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].I64Column[c.columnIndex].Value
			}
		} else if columnType == "FLOAT_TYPE" {
			if c.queue[i].DoubleColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].DoubleColumn[c.columnIndex].Value
			}
		} else if columnType == "DOUBLE_TYPE" {
			if c.queue[i].DoubleColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].DoubleColumn[c.columnIndex].Value
			}
		} else if columnType == "STRING_TYPE" {
			if c.queue[i].StringColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].StringColumn[c.columnIndex].Value
			}
		} else if columnType == "TIMESTAMP_TYPE" {
			if c.queue[i].StringColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].StringColumn[c.columnIndex].Value
			}
		} else if columnType == "ARRAY_TYPE" {
			if c.queue[i].StringColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].StringColumn[c.columnIndex].Value
			}
		} else if columnType == "MAP_TYPE" {
			if c.queue[i].StringColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].StringColumn[c.columnIndex].Value
			}
		} else if columnType == "STRUCT_TYPE" {
			if c.queue[i].StringColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].StringColumn[c.columnIndex].Value
			}
		} else if columnType == "UNION_TYPE" {
			if c.queue[i].StringColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].StringColumn[c.columnIndex].Value
			}
		} else if columnType == "DECIMAL_TYPE" {
			if c.queue[i].StringColumn[c.columnIndex] == nil {
				m[columnName] = nil
			} else {
				m[columnName] = c.queue[i].StringColumn[c.columnIndex].Value
			}
		}
	}
	if len(m) != len(d) {
		log.Printf("Some columns have the same name as per the description: %v, this makes it impossible to get the values using the RowMap API, please use the FetchOne API", d)
	}
	c.columnIndex++
	return m
}

// FetchOne returns one row and advances the cursor one
func (c *Cursor) FetchOne(ctx context.Context, dests ...interface{}) {
	c.Err = nil
	c.fetchIfEmpty(ctx)
	if c.Err != nil {
		return
	}

	if len(c.queue) != len(dests) {
		c.Err = fmt.Errorf("%d arguments where passed for filling but the number of columns is %d", len(dests), len(c.queue))
		return
	}
	for i := 0; i < len(c.queue); i++ {
		if c.queue[i].IsSetByteColumn() {
			if dests[i] == nil {
				dests[i] = c.queue[i].ByteColumn[c.columnIndex].Value
				continue
			}
			d, ok := dests[i].(*int8)
			if !ok {
				d, ok := dests[i].(**int8)
				if !ok {
					c.Err = fmt.Errorf("Unexpected data type %T for value %v (should be %T)", dests[i], c.queue[i].ByteColumn[c.columnIndex].Value, c.queue[i].ByteColumn[c.columnIndex].Value)
					return
				}

				if c.queue[i].ByteColumn[c.columnIndex] == nil {
					*d = nil
				} else {
					**d = *c.queue[i].ByteColumn[c.columnIndex].Value
				}
			} else {
				*d = *c.queue[i].ByteColumn[c.columnIndex].Value
			}

		} else if c.queue[i].IsSetI16Column() {
			if dests[i] == nil {
				dests[i] = c.queue[i].I16Column[c.columnIndex].Value
				continue
			}
			d, ok := dests[i].(*int16)
			if !ok {
				d, ok := dests[i].(**int16)
				if !ok {
					c.Err = fmt.Errorf("Unexpected data type %T for value %v (should be %T)", dests[i], c.queue[i].I16Column[c.columnIndex].Value, c.queue[i].I16Column[c.columnIndex].Value)
					return
				}

				if c.queue[i].I16Column[c.columnIndex] == nil {
					*d = nil
				} else {
					**d = *c.queue[i].I16Column[c.columnIndex].Value
				}
			} else {
				*d = *c.queue[i].I16Column[c.columnIndex].Value
			}
		} else if c.queue[i].IsSetI32Column() {
			if dests[i] == nil {
				dests[i] = c.queue[i].I32Column[c.columnIndex].Value
				continue
			}
			d, ok := dests[i].(*int32)
			if !ok {
				d, ok := dests[i].(**int32)
				if !ok {
					c.Err = fmt.Errorf("Unexpected data type %T for value %v (should be %T)", dests[i], c.queue[i].I32Column[c.columnIndex].Value, c.queue[i].I32Column[c.columnIndex].Value)
					return
				}

				if c.queue[i].I32Column[c.columnIndex] == nil {
					*d = nil
				} else {
					**d = *c.queue[i].I32Column[c.columnIndex].Value
				}
			} else {
				*d = *c.queue[i].I32Column[c.columnIndex].Value
			}
		} else if c.queue[i].IsSetI64Column() {
			if dests[i] == nil {
				dests[i] = c.queue[i].I64Column[c.columnIndex].Value
				continue
			}
			d, ok := dests[i].(*int64)
			if !ok {
				d, ok := dests[i].(**int64)
				if !ok {
					c.Err = fmt.Errorf("Unexpected data type %T for value %v (should be %T)", dests[i], c.queue[i].I64Column[c.columnIndex].Value, c.queue[i].I64Column[c.columnIndex].Value)
					return
				}

				if c.queue[i].I64Column[c.columnIndex] == nil {
					*d = nil
				} else {
					**d = *c.queue[i].I64Column[c.columnIndex].Value
				}
			} else {
				*d = *c.queue[i].I64Column[c.columnIndex].Value
			}
		} else if c.queue[i].IsSetStringColumn() {
			if dests[i] == nil {
				dests[i] = c.queue[i].StringColumn[c.columnIndex].Value
				continue
			}
			d, ok := dests[i].(*string)
			if !ok {
				d, ok := dests[i].(**string)
				if !ok {
					c.Err = fmt.Errorf("Unexpected data type %T for value %v (should be %T)", dests[i], c.queue[i].StringColumn[c.columnIndex].Value, c.queue[i].StringColumn[c.columnIndex].Value)
					return
				}

				if c.queue[i].StringColumn[c.columnIndex] == nil {
					*d = nil
				} else {
					**d = *c.queue[i].StringColumn[c.columnIndex].Value
				}
			} else {
				*d = *c.queue[i].StringColumn[c.columnIndex].Value
			}
		} else if c.queue[i].IsSetDoubleColumn() {
			if dests[i] == nil {
				dests[i] = c.queue[i].DoubleColumn[c.columnIndex].Value
				continue
			}
			d, ok := dests[i].(*float64)
			if !ok {
				d, ok := dests[i].(**float64)
				if !ok {
					c.Err = fmt.Errorf("Unexpected data type %T for value %v (should be %T)", dests[i], c.queue[i].DoubleColumn[c.columnIndex].Value, c.queue[i].DoubleColumn[c.columnIndex].Value)
					return
				}

				if c.queue[i].DoubleColumn[c.columnIndex] == nil {
					*d = nil
				} else {
					**d = *c.queue[i].DoubleColumn[c.columnIndex].Value
				}
			} else {
				*d = *c.queue[i].DoubleColumn[c.columnIndex].Value
			}
		} else if c.queue[i].IsSetBoolColumn() {
			if dests[i] == nil {
				dests[i] = c.queue[i].BoolColumn[c.columnIndex]
				continue
			}
			d, ok := dests[i].(*bool)
			if !ok {
				d, ok := dests[i].(**bool)
				if !ok {
					c.Err = fmt.Errorf("Unexpected data type %T for value %v (should be %T)", dests[i], c.queue[i].BoolColumn[c.columnIndex].Value, c.queue[i].BoolColumn[c.columnIndex].Value)
					return
				}

				if c.queue[i].BoolColumn[c.columnIndex] == nil {
					*d = nil
				} else {
					**d = *c.queue[i].BoolColumn[c.columnIndex].Value
				}
			} else {
				*d = *c.queue[i].BoolColumn[c.columnIndex].Value
			}
		} else {
			c.Err = fmt.Errorf("Empty column %v", c.queue[i])
			return
		}
	}
	c.columnIndex++

	return
}

func isNull(nulls []byte, position int) bool {
	index := position / 8
	if len(nulls) > index {
		b := nulls[index]
		return (b & (1 << (uint)(position%8))) != 0
	}
	return false
}

// Description return a map with the names of the columns and their types
// must be called after a FetchResult request
// a context should be added here but seems to be ignored by thrift
func (c *Cursor) Description() [][]string {
	if c.description != nil {
		return c.description
	}
	if c.operationHandle == nil {
		c.Err = fmt.Errorf("Description can only be called after after a Poll or after an async request")
	}

	metaRequest := hiveserver.NewTGetResultSetMetadataReq()
	metaRequest.OperationHandle = c.operationHandle
	metaResponse, err := c.conn.client.GetResultSetMetadata(context.Background(), metaRequest)
	if err != nil {
		c.Err = err
		return nil
	}
	if metaResponse.Status.StatusCode != hiveserver.TStatusCode_SUCCESS_STATUS {
		c.Err = fmt.Errorf(metaResponse.Status.String())
		return nil
	}
	m := make([][]string, len(metaResponse.Schema.Columns))
	for i, column := range metaResponse.Schema.Columns {
		for _, typeDesc := range column.TypeDesc.Types {
			m[i] = []string{column.ColumnName, typeDesc.PrimitiveEntry.Type.String()}
		}
	}
	c.description = m
	return m
}

// HasMore returns weather more rows can be fetched from the server
func (c *Cursor) HasMore(ctx context.Context) bool {
	c.Err = nil
	if c.response == nil && c.state != _FINISHED {
		c.Err = c.pollUntilData(ctx, 1)
		return c.state != _FINISHED || c.totalRows != c.columnIndex
	}
	// *c.response.HasMoreRows is always false
	// so it can be checked and another roundtrip has to be done if etra data has been added
	if c.totalRows == c.columnIndex && c.state != _FINISHED {
		c.Err = c.pollUntilData(ctx, 1)
	}

	return c.state != _FINISHED || c.totalRows != c.columnIndex
}

func (c *Cursor) Error() error {
	return c.Err
}

func (c *Cursor) pollUntilData(ctx context.Context, n int) (err error) {
	rowsAvailable := make(chan error)
	var stopLock sync.Mutex
	var done = false
	go func() {
		defer close(rowsAvailable)
		for true {
			stopLock.Lock()
			if done {
				stopLock.Unlock()
				rowsAvailable <- nil
				return
			}
			stopLock.Unlock()

			fetchRequest := hiveserver.NewTFetchResultsReq()
			fetchRequest.OperationHandle = c.operationHandle
			fetchRequest.Orientation = hiveserver.TFetchOrientation_FETCH_NEXT
			fetchRequest.MaxRows = c.conn.configuration.FetchSize
			responseFetch, err := c.conn.client.FetchResults(context.Background(), fetchRequest)
			if err != nil {
				rowsAvailable <- err
				return
			}
			c.response = responseFetch

			if responseFetch.Status.StatusCode != hiveserver.TStatusCode_SUCCESS_STATUS {
				rowsAvailable <- fmt.Errorf(responseFetch.Status.String())
				return
			}
			err = c.parseResults(responseFetch)
			if err != nil {
				rowsAvailable <- err
				return
			}

			if len(c.queue) > 0 {
				rowsAvailable <- nil
				return
			}
			time.Sleep(time.Duration(c.conn.configuration.PollIntervalInMillis) * time.Millisecond)
		}
	}()

	select {
	case err = <-rowsAvailable:
	case <-ctx.Done():
		stopLock.Lock()
		done = true
		stopLock.Unlock()
		select {
		// Wait for goroutine to finish
		case <-rowsAvailable:
		}
		err = fmt.Errorf("Context is done")
	}

	if err != nil {
		return err
	}

	if len(c.queue) < n {
		return fmt.Errorf("Only %d rows where received", len(c.queue))
	}
	return nil
}

// Cancels the current operation
func (c *Cursor) Cancel() {
	c.Err = nil
	cancelRequest := hiveserver.NewTCancelOperationReq()
	cancelRequest.OperationHandle = c.operationHandle
	var responseCancel *hiveserver.TCancelOperationResp
	// This context is simply ignored
	responseCancel, c.Err = c.conn.client.CancelOperation(context.Background(), cancelRequest)
	if c.Err != nil {
		return
	}
	if !success(responseCancel.GetStatus()) {
		c.Err = fmt.Errorf("Error closing the operation: %s", responseCancel.Status.String())
	}
	return
}

// Close close the cursor
func (c *Cursor) Close() {
	c.Err = c.resetState()
}

func (c *Cursor) resetState() error {
	c.response = nil
	c.Err = nil
	c.queue = nil
	c.columnIndex = 0
	c.totalRows = 0
	c.state = _NONE
	c.description = nil
	c.newData = false
	if c.operationHandle != nil {
		closeRequest := hiveserver.NewTCloseOperationReq()
		closeRequest.OperationHandle = c.operationHandle
		// This context is ignored
		responseClose, err := c.conn.client.CloseOperation(context.Background(), closeRequest)
		c.operationHandle = nil
		if err != nil {
			return err
		}
		if !success(responseClose.GetStatus()) {
			return fmt.Errorf("Error closing the operation: %s", responseClose.Status.String())
		}
		return nil
	}
	return nil
}

func (c *Cursor) parseResults(response *hiveserver.TFetchResultsResp) (err error) {
	c.queue = response.Results.GetColumns()
	c.columnIndex = 0
	c.totalRows, err = getTotalRows(c.queue)
	c.newData = c.totalRows > 0
	if !c.newData {
		c.state = _FINISHED
	}
	return
}

func getTotalRows(columns []*hiveserver.TColumn) (int, error) {
	for _, el := range columns {
		if el.IsSetByteColumn() {
			return len(el.ByteColumn), nil
		} else if el.IsSetI16Column() {
			return len(el.I16Column), nil
		} else if el.IsSetI32Column() {
			return len(el.I32Column), nil
		} else if el.IsSetI64Column() {
			return len(el.I64Column), nil
		} else if el.IsSetBoolColumn() {
			return len(el.BoolColumn), nil
		} else if el.IsSetDoubleColumn() {
			return len(el.DoubleColumn), nil
		} else if el.IsSetStringColumn() {
			return len(el.StringColumn), nil
		} else {
			return -1, fmt.Errorf("Unrecognized column type %T", el)
		}
	}
	return 0, fmt.Errorf("All columns seem empty")
}

type inMemoryCookieJar struct {
	given   *bool
	storage map[string][]http.Cookie
}

func (jar inMemoryCookieJar) SetCookies(u *url.URL, cookies []*http.Cookie) {
	for _, cookie := range cookies {
		jar.storage["cliservice"] = []http.Cookie{*cookie}
	}
	*jar.given = false
}

func (jar inMemoryCookieJar) Cookies(u *url.URL) []*http.Cookie {
	cookiesArray := []*http.Cookie{}
	for pattern, cookies := range jar.storage {
		if strings.Contains(u.String(), pattern) {
			for i := range cookies {
				cookiesArray = append(cookiesArray, &cookies[i])
			}
		}
	}
	if !*jar.given {
		*jar.given = true
		return cookiesArray
	} else {
		return nil
	}
}

func newCookieJar() inMemoryCookieJar {
	storage := make(map[string][]http.Cookie)
	f := false
	return inMemoryCookieJar{&f, storage}
}
