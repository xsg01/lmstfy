package redis

import (
	"encoding/binary"
	"errors"
	"time"

	go_redis "github.com/go-redis/redis"
	"github.com/meitu/lmstfy/engine"
	"github.com/sirupsen/logrus"
)

type QueueName struct {
	Namespace string
	Queue     string
}

func (k *QueueName) String() string {
	return join(QueuePrefix, k.Namespace, k.Queue)
}

func (k *QueueName) Decode(str string) error {
	splits := splits(3, str)
	if len(splits) != 3 || splits[0] != QueuePrefix {
		return errors.New("invalid format")
	}
	k.Namespace = splits[1]
	k.Queue = splits[2]
	return nil
}

// Queue is the "ready queue" that has all the jobs that can be consumed right now
type Queue struct {
	name  QueueName
	redis *RedisInstance
	timer *Timer

	lua_destroy_sha string
}

func NewQueue(namespace, queue string, redis *RedisInstance, timer *Timer) *Queue {
	return &Queue{
		name:  QueueName{Namespace: namespace, Queue: queue},
		redis: redis,
		timer: timer,

		// NOTE: deadletter and queue are actually the same data structure, we could reuse the lua script
		// to empty the redis list (used as queue here). all we need to do is pass the queue name as the
		// deadletter name.
		lua_destroy_sha: _lua_delete_deadletter_sha,
	}
}

func (q *Queue) Name() string {
	return q.name.String()
}

// Push a job into the queue, the job data format: {tries}{job id}
func (q *Queue) Push(j engine.Job, tries uint16) error {
	if tries == 0 {
		return nil
	}
	if j.Namespace() != q.name.Namespace || j.Queue() != q.name.Queue {
		// Wrong queue for the job
		return engine.ErrWrongQueue
	}
	metrics.queueDirectPushJobs.WithLabelValues(q.redis.Name).Inc()
	val := structPack(tries, j.ID())
	return q.redis.Conn.LPush(q.Name(), val).Err()
}

// Pop a job. If the tries > 0, add job to the "in-flight" timer with timestamp
// set to `TTR + now()`; Or we might just move the job to "dead-letter".
func (q *Queue) Poll(timeoutSecond, ttrSecond uint32) (jobID string, err error) {
	_, jobID, err = PollQueues(q.redis, q.timer, []QueueName{q.name}, timeoutSecond, ttrSecond)
	return jobID, err
}

// Return number of the current in-queue jobs
func (q *Queue) Size() (size int64, err error) {
	return q.redis.Conn.LLen(q.name.String()).Result()
}

// Peek a right-most element in the list without popping it
func (q *Queue) Peek() (jobID string, err error) {
	val, err := q.redis.Conn.LIndex(q.Name(), -1).Result()
	switch err {
	case nil:
		// continue
	case go_redis.Nil:
		return "", engine.ErrNotFound
	default:
		return "", err
	}
	_, jobID, err = structUnpack(val)
	return jobID, err
}

func (q *Queue) Destroy() (count int64, err error) {
	poolPrefix := PoolJobKeyPrefix(q.name.Namespace, q.name.Queue)
	var batchSize int64 = 100
	for {
		val, err := q.redis.Conn.EvalSha(q.lua_destroy_sha, []string{q.Name(), poolPrefix}, batchSize).Result()
		if err != nil {
			if isLuaScriptGone(err) {
				PreloadDeadLetterLuaScript(q.redis)
				continue
			}
			return count, err
		}
		n, _ := val.(int64)
		count += n
		if n < batchSize { // Dead letter is empty
			break
		}
	}
	return count, nil
}

// Poll from multiple queues using blocking method; OR pop a job from one queue using non-blocking method
func PollQueues(redis *RedisInstance, timer *Timer, queueNames []QueueName, timeoutSecond, ttrSecond uint32) (queueName *QueueName, jobID string, err error) {
	defer func() {
		if jobID != "" {
			metrics.queuePopJobs.WithLabelValues(redis.Name).Inc()
		}
	}()
	var val []string
	if timeoutSecond > 0 { // Blocking poll
		keys := make([]string, len(queueNames))
		for i, k := range queueNames {
			keys[i] = k.String()
		}
		val, err = redis.Conn.BRPop(time.Duration(timeoutSecond)*time.Second, keys...).Result()
	} else { // Non-Blocking fetch
		if len(queueNames) != 1 {
			panic("non-blocking pop can NOT support multiple keys")
		}
		val = make([]string, 2)
		val[0] = queueNames[0].String() // Just to be coherent with BRPop return values
		val[1], err = redis.Conn.RPop(val[0]).Result()
	}
	switch err {
	case nil:
		// continue
	case go_redis.Nil:
		logger.Debug("Job not found")
		return nil, "", nil
	default:
		logger.WithField("err", err).Error("Failed to pop job from queue")
		return nil, "", err
	}
	queueName = &QueueName{}
	if err := queueName.Decode(val[0]); err != nil {
		logger.WithField("err", err).Error("Failed to decode queue name")
		return nil, "", err
	}
	tries, jobID, err := structUnpack(val[1])
	if err != nil {
		logger.WithField("err", err).Error("Failed to unpack lua struct data")
		return nil, "", err
	}

	if tries == 0 {
		panic("tries == 0, job with tries == 0 should never appears here")
	}

	err = timer.Add(queueName.Namespace, queueName.Queue, jobID, ttrSecond, tries-1) // NOTE: tries is decreased
	if err != nil {
		logger.WithFields(logrus.Fields{
			"err":   err,
			"jobID": jobID,
			"ttr":   ttrSecond,
			"queue": queueName.String(),
		}).Error("Failed to add job to timer for ttr")
		return queueName, jobID, err
	}
	return queueName, jobID, nil
}

// Pack (tries, jobID) into lua struct pack of format "HHHc0", in lua this can be done:
//   ```local data = struct.pack("HHc0", tries, #job_id, job_id)```
func structPack(tries uint16, jobID string) (data string) {
	buf := make([]byte, 2+2+len(jobID))
	binary.LittleEndian.PutUint16(buf[0:], tries)
	binary.LittleEndian.PutUint16(buf[2:], uint16(len(jobID)))
	copy(buf[4:], jobID)
	return string(buf)
}

// Unpack the "HHc0" lua struct format, in lua this can be done:
//   ```local tries, job_id = struct.unpack("HHc0", data)```
func structUnpack(data string) (tries uint16, jobID string, err error) {
	buf := []byte(data)
	h1 := binary.LittleEndian.Uint16(buf[0:])
	h2 := binary.LittleEndian.Uint16(buf[2:])
	jobID = string(buf[4:])
	tries = h1
	if len(jobID) != int(h2) {
		err = errors.New("corrupted data")
	}
	return
}
