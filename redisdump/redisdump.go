package redisdump

import (
	"bufio"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	radix "github.com/mediocregopher/radix.v3"
)

func min(a, b int) int {
	if a <= b {
		return a
	}
	return b
}

func ttlToRedisCmd(k string, val int64) []string {
	return []string{"EXPIREAT", k, fmt.Sprint(time.Now().Unix() + val)}
}

func stringToRedisCmd(k, val string) []string {
	return []string{"SET", k, val}
}

func hashToRedisCmd(k string, val map[string]string) []string {
	cmd := []string{"HSET", k}
	for k, v := range val {
		cmd = append(cmd, k, v)
	}
	return cmd
}

func setToRedisCmd(k string, val []string) []string {
	cmd := []string{"SADD", k}
	return append(cmd, val...)
}

func listToRedisCmd(k string, val []string) []string {
	cmd := []string{"RPUSH", k}
	return append(cmd, val...)
}

func zsetToRedisCmd(k string, val []string) []string {
	cmd := []string{"ZADD", k}
	var key string

	for i, v := range val {
		if i%2 == 0 {
			key = v
			continue
		}

		cmd = append(cmd, v, key)
	}
	return cmd
}

// RESPSerializer will serialize cmd to RESP
func RESPSerializer(cmd []string) string {
	s := ""
	s += "*" + strconv.Itoa(len(cmd)) + "\r\n"
	for _, arg := range cmd {
		s += "$" + strconv.Itoa(len(arg)) + "\r\n"
		s += arg + "\r\n"
	}
	return s
}

// RedisCmdSerializer will serialize cmd to a string with redis commands
func RedisCmdSerializer(cmd []string) string {
	return strings.Join(cmd, " ")
}

func dumpKeys(client radix.Client, keys []string, logger *log.Logger, serializer func([]string) string) error {
	var err error
	var redisCmd []string
	var withTTL = true

	for _, key := range keys {
		var keyType string

		err = client.Do(radix.Cmd(&keyType, "TYPE", key))
		if err != nil {
			return err
		}

		switch keyType {
		case "string":
			var val string
			if err = client.Do(radix.Cmd(&val, "GET", key)); err != nil {
				return err
			}
			redisCmd = stringToRedisCmd(key, val)

		case "list":
			var val []string
			if err = client.Do(radix.Cmd(&val, "LRANGE", key, "0", "-1")); err != nil {
				return err
			}
			redisCmd = listToRedisCmd(key, val)

		case "set":
			var val []string
			if err = client.Do(radix.Cmd(&val, "SMEMBERS", key)); err != nil {
				return err
			}
			redisCmd = setToRedisCmd(key, val)

		case "hash":
			var val map[string]string
			if err = client.Do(radix.Cmd(&val, "HGETALL", key)); err != nil {
				return err
			}
			redisCmd = hashToRedisCmd(key, val)

		case "zset":
			var val []string
			if err = client.Do(radix.Cmd(&val, "ZRANGEBYSCORE", key, "-inf", "+inf", "WITHSCORES")); err != nil {
				return err
			}
			redisCmd = zsetToRedisCmd(key, val)

		case "none":

		default:
			return fmt.Errorf("Key %s is of unreconized type %s", key, keyType)
		}

		logger.Print(serializer(redisCmd))

		if withTTL {
			var ttl int64
			if err = client.Do(radix.Cmd(&ttl, "TTL", key)); err != nil {
				return err
			}
			if ttl > 0 {
				redisCmd = ttlToRedisCmd(key, ttl)
				logger.Printf(serializer(redisCmd))
			}
		}
	}

	return nil
}

func dumpKeysWorker(client radix.Client, keyBatches <-chan []string, logger *log.Logger, serializer func([]string) string, errors chan<- error, done chan<- bool) {
	for keyBatch := range keyBatches {
		if err := dumpKeys(client, keyBatch, logger, serializer); err != nil {
			errors <- err
		}
	}
	done <- true
}

// ProgressNotification message indicates the progress in dumping the Redis server,
// and can be used to provide a progress visualisation such as a progress bar.
// Done is the number of items dumped, Total is the total number of items to dump.
type ProgressNotification struct {
	Done, Total int
}

func parseKeyspaceInfo(keyspaceInfo string) ([]uint8, error) {
	var dbs []uint8

	scanner := bufio.NewScanner(strings.NewReader(keyspaceInfo))

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		if !strings.HasPrefix(line, "db") {
			continue
		}

		dbIndexString := line[2:strings.IndexAny(line, ":")]
		dbIndex, err := strconv.ParseUint(dbIndexString, 10, 8)
		if err != nil {
			return nil, err
		}
		if dbIndex > 16 {
			return nil, fmt.Errorf("Error parsing INFO keyspace")
		}

		dbs = append(dbs, uint8(dbIndex))
	}

	return dbs, nil
}

func getDBIndexes(redisURL string) ([]uint8, error) {
	client, err := radix.NewPool("tcp", redisURL, 1)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	var keyspaceInfo string
	if err = client.Do(radix.Cmd(&keyspaceInfo, "INFO", "keyspace")); err != nil {
		return nil, err
	}

	return parseKeyspaceInfo(keyspaceInfo)
}

func withDBSelection(dial radix.ConnFunc, db uint8) radix.ConnFunc {
	return func(network, addr string) (radix.Conn, error) {
		conn, err := dial(network, addr)
		if err != nil {
			return nil, err
		}

		if err := conn.Do(radix.Cmd(nil, "SELECT", fmt.Sprint(db))); err != nil {
			conn.Close()
			return nil, err
		}

		return conn, nil
	}
}

// DumpDB dumps all keys from a single Redis DB
func DumpDB(redisURL string, db uint8, nWorkers int, logger *log.Logger, serializer func([]string) string, progress chan<- ProgressNotification) error {
	var err error

	errors := make(chan error)
	nErrors := 0
	go func() {
		for err := range errors {
			fmt.Fprintln(os.Stderr, "Error: "+err.Error())
			nErrors++
		}
	}()

	client, err := radix.NewPool("tcp", redisURL, nWorkers, radix.PoolConnFunc(withDBSelection(radix.Dial, db)))
	if err != nil {
		return err
	}
	defer client.Close()

	if err = client.Do(radix.Cmd(nil, "SELECT", fmt.Sprint(db))); err != nil {
		return err
	}
	logger.Printf(serializer([]string{"SELECT", fmt.Sprint(db)}))

	var keys []string
	if err = client.Do(radix.Cmd(&keys, "KEYS", "*")); err != nil {
		return err
	}

	done := make(chan bool)
	keyBatches := make(chan []string)
	for i := 0; i < nWorkers; i++ {
		go dumpKeysWorker(client, keyBatches, logger, serializer, errors, done)
	}

	batchSize := 100
	for i := 0; i < len(keys) && nErrors == 0; i += batchSize {
		batchEnd := min(i+batchSize, len(keys))
		keyBatches <- keys[i:batchEnd]
		if progress != nil {
			progress <- ProgressNotification{batchEnd, len(keys)}
		}
	}

	close(keyBatches)

	for i := 0; i < nWorkers; i++ {
		<-done
	}

	return nil
}

// DumpServer dumps all Keys from the redis server given by redisURL,
// to the Logger logger. Progress notification informations
// are regularly sent to the channel progressNotifications
func DumpServer(redisURL string, nWorkers int, logger *log.Logger, serializer func([]string) string, progress chan<- ProgressNotification) error {
	dbs, err := getDBIndexes(redisURL)
	if err != nil {
		return err
	}

	for _, db := range dbs {
		if err = DumpDB(redisURL, db, nWorkers, logger, serializer, progress); err != nil {
			return err
		}
	}

	return nil
}
