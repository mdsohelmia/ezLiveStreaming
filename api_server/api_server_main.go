// The API server for handling live streaming job requests
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"ezliveStreaming/demo" // Demo ONLY!!!
	"ezliveStreaming/job"
	"ezliveStreaming/job_sqs"
	"ezliveStreaming/models"
	"ezliveStreaming/redis_client"
	"flag"
	"fmt"
	"github.com/google/uuid"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type SqsConfig struct {
	Queue_name string
}

type ApiServerConfig struct {
	Api_server_hostname string
	Api_server_port     string
	Log_path            string
	Drm_key_server_url  string
	Sqs                 SqsConfig
	Redis               redis_client.RedisConfig
}

var liveJobsEndpoint = "jobs"

func assignJobInputStreamId() string {
	return uuid.New().String()
}

func createDrmKey(jid string) (models.CreateKeyResponse, error) {
	var ckreq models.CreateKeyRequest
	var ckresp models.CreateKeyResponse
	ckreq.Content_id = jid
	b, _ := json.Marshal(ckreq)
	req, err := http.NewRequest(http.MethodPost, server_config.Drm_key_server_url, bytes.NewReader(b))
	if err != nil {
		Log.Println("Error: Failed to POST to: ", server_config.Drm_key_server_url)
		// TODO: Need to retry when failed
		return ckresp, errors.New("StatusReportFailure_fail_to_post")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		Log.Println("Failed to POST to: ", server_config.Drm_key_server_url)
		return ckresp, err
	}

	// TODO: Need to handle error response (other than http code 201)
	if resp.StatusCode != http.StatusCreated {
		Log.Println("Bad response from DRM key server for CreateKeyRequest")
		return ckresp, errors.New("StatusReportFailure_fail_to_read_key_server_response")
	}

	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		Log.Println("Error: Failed to read response body (createDrmKey)")
		return ckresp, errors.New("http_post_response_parsing_failure")
	}

	json.Unmarshal(bodyBytes, &ckresp)
	return ckresp, nil
}

func getDrmKey(key_id string) (models.KeyInfo, error) {
	var k models.KeyInfo
	req, err := http.NewRequest(http.MethodGet, server_config.Drm_key_server_url+key_id, nil)
	if err != nil {
		Log.Println("Error: Failed to GET: ", server_config.Drm_key_server_url)
		// TODO: Need to retry when failed
		return k, errors.New("StatusReportFailure_fail_to_get")
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		Log.Println("Failed to GET: ", server_config.Drm_key_server_url)
		return k, err
	}

	// TODO: Need to handle error response (other than http code 200)
	if resp.StatusCode != http.StatusOK {
		Log.Println("Bad response from DRM key server for GetKeyRequest")
		return k, errors.New("StatusReportFailure_fail_to_read_key_server_response")
	}

	defer resp.Body.Close()
	bodyBytes, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		Log.Println("Error: Failed to read response body (getDrmKey)")
		return k, errors.New("http_post_response_parsing_failure")
	}

	json.Unmarshal(bodyBytes, &k)
	return k, nil
}

func createJob(j job.LiveJobSpec, warnings []string) (error, job.LiveJob) {
	var lj job.LiveJob
	lj.Id = uuid.New().String()
	Log.Println("Generating a random job ID: ", lj.Id)

	lj.StreamKey = assignJobInputStreamId()
	if j.Output.Stream_type == job.DASH {
		// Example: https://bozhang-private.s3.amazonaws.com/output_70255156-26ef-4378-811b-dfc44a7c6cb5/master.mpd
		lj.Playback_url = "https://" + j.Output.S3_output.Bucket + ".s3.amazonaws.com/output_" + lj.Id + "/" + job.DASH_MPD_FILE_NAME
	} else if j.Output.Stream_type == job.HLS {
		lj.Playback_url = "https://" + j.Output.S3_output.Bucket + ".s3.amazonaws.com/output_" + lj.Id + "/" + job.HLS_MASTER_PLAYLIST_FILE_NAME
	}

	lj.Spec = j
	lj.Time_created = time.Now()
	lj.Stop = false   // Set to true when the client wants to stop this job
	lj.Delete = false // Set to true when the client wants to delete this job
	lj.State = job.JOB_STATE_CREATED

	warning_message := "Warnings: "
	for _, e := range warnings {
		warning_message += e
	}

	lj.Job_validation_warnings = warning_message

	var k models.KeyInfo
	var err_get_key error
	if j.Output.Drm.Protection_system != "" {
		create_key_resp, err_create_key := createDrmKey(lj.Id)
		if err_create_key == nil {
			k, err_get_key = getDrmKey(create_key_resp.Key_id)
			if err_get_key == nil {
				lj.DrmEncryptionKeyInfo = k
				Log.Printf("DRM Key INFO: key_id: %s, key: %s, content_id: %s\n", lj.DrmEncryptionKeyInfo.Key_id, lj.DrmEncryptionKeyInfo.Key, lj.DrmEncryptionKeyInfo.Content_id)
			} else {
				Log.Printf("Failed to get DRM info. Do not protect the live stream\n")
			}
		} else {
			Log.Printf("Failed to create DRM key. Do not protect the live stream\n")
		}
	} else {
		Log.Printf("DRM is not configured. Do not protect the live stream\n")
	}

	e := createUpdateJob(lj)
	if e != nil {
		Log.Println("Error: Failed to create/update job ID: ", lj.Id)
		return e, lj
	}

	j2, err := getJobById(lj.Id)
	if err != nil {
		Log.Println("Error: Failed to find job ID: ", lj.Id)
		return e, lj
	}

	Log.Printf("New job created: %+v\n", j2)
	return nil, j2
}

func createUpdateJob(j job.LiveJob) error {
	err := redis.HSetStruct(redis_client.REDIS_KEY_ALLJOBS, j.Id, j)
	if err != nil {
		Log.Println("Failed to update job id=", j.Id, ". Error: ", err)
	}

	return err
}

func stopJob(j job.LiveJob) error {
	j.Stop = true
	err := redis.HSetStruct(redis_client.REDIS_KEY_ALLJOBS, j.Id, j)
	if err != nil {
		Log.Println("Failed to stop job id=", j.Id, ". Error: ", err)
	}

	return err
}

func resumeJob(j job.LiveJob) error {
	j.Stop = false
	err := redis.HSetStruct(redis_client.REDIS_KEY_ALLJOBS, j.Id, j)
	if err != nil {
		Log.Println("Failed to stop job id=", j.Id, ". Error: ", err)
	}

	return err
}

func deleteJob(j job.LiveJob) error {
	err := redis.HDelOne(redis_client.REDIS_KEY_ALLJOBS, j.Id)
	if err != nil {
		Log.Println("Failed to stop job id=", j.Id, ". Error: ", err)
	}

	return err
}

func getJobById(jid string) (job.LiveJob, error) {
	var j job.LiveJob
	v, err := redis.HGet(redis_client.REDIS_KEY_ALLJOBS, jid)
	if err != nil {
		Log.Println("Failed to find job id=", jid, ". Error: ", err)
		return j, err
	}

	err = json.Unmarshal([]byte(v), &j)
	if err != nil {
		Log.Println("Failed to unmarshal Redis result (getJobById). Error: ", err)
		return j, err
	}

	return j, nil
}

func getAllJobs() ([]job.LiveJob, error) {
	var jobs []job.LiveJob
	jobsString, err := redis.HGetAll(redis_client.REDIS_KEY_ALLJOBS)
	if err != nil {
		Log.Println("Failed to get all jobs. Error: ", err)
		return jobs, err
	}

	var j job.LiveJob
	for _, j_string := range jobsString {
		err = json.Unmarshal([]byte(j_string), &j)
		if err != nil {
			jobs = nil
			Log.Println("Failed to unmarshal Redis results (getAllJobs). Error: ", err)
			return jobs, err
		}

		jobs = append(jobs, j)
	}

	return jobs, nil
}

func getAllActiveJobs() ([]job.LiveJob, error) {
	var jobs []job.LiveJob
	jobsString, err := redis.HGetAll(redis_client.REDIS_KEY_ALLJOBS)
	if err != nil {
		Log.Println("Failed to get all jobs. Error: ", err)
		return jobs, err
	}

	var j job.LiveJob
	for _, j_string := range jobsString {
		err = json.Unmarshal([]byte(j_string), &j)
		if err != nil {
			jobs = nil
			Log.Println("Failed to unmarshal Redis results (getAllActiveJobs). Error: ", err)
			return jobs, err
		}

		if j.State == job.JOB_STATE_RUNNING || j.State == job.JOB_STATE_STREAMING {
			jobs = append(jobs, j)
		}
	}

	return jobs, nil
}

var isLiveFeeding = false
var liveFeedCmd *exec.Cmd

// Demo only!!!
// E.g., ffmpeg -stream_loop -1 -re -i ed_720p_english_audio.mp4 -c:v copy -c:a copy -f flv rtmp://global-ingest-live.visionular.com/live/202309-a83383e4-7db7-402d-995f-32d8c447f89e
//       ffmpeg -i in.mp4 -vf "drawtext=fontfile=/usr/share/fonts/truetype/DroidSans.ttf: timecode='09\:57\:00\:00': r=25: \x=(w-tw)/2: y=h-(2*lh): fontcolor=white: box=1: boxcolor=0x00000000@1" -an
func start_ffmpeg_live_contribution(spec demo.CreateLiveFeedSpec) error {
	if isLiveFeeding {
		Log.Println("Live feeder is already up")
		return errors.New("DuplicateLiveFeeding")
	}

	liveFeedCmd = exec.Command("ffmpeg", "-stream_loop", "-1", "-re", "-i", "/tmp/1.mp4", "-c", "copy", "-f", "flv", spec.RtmpIngestUrl)

	Log.Println("!!!Starting live feeding...")
	go func() {
		err := liveFeedCmd.Start()
		if err != nil {
			Log.Println("Could not start live feeding: ", err)
			return
		} else {
			isLiveFeeding = true
		}
	}()

	return nil
}

func stop_ffmpeg_live_contribution() {
	if liveFeedCmd == nil {
		Log.Println("Live feeder isn't running")
		return
	}

	process, err1 := os.FindProcess(int(liveFeedCmd.Process.Pid))
	if err1 != nil {
		Log.Printf("Process id = %d not found. Error: %v\n", liveFeedCmd.Process.Pid, err1)
		return
	} else {
		err2 := process.Signal(syscall.Signal(syscall.SIGINT))
		Log.Printf("process.Signal.SIGINT on pid %d returned: %v\n", liveFeedCmd.Process.Pid, err2)
		if err2 == nil {
			isLiveFeeding = false
			Log.Println("Live feed stopped successfully!")
		}
	}
}

func main_server_handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=ascii")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type,access-control-allow-origin, access-control-allow-headers")
	w.Header().Set("Access-Control-Allow-Methods", "*")

	if (*r).Method == "OPTIONS" {
		return
	}

	Log.Println("----------------------------------------")
	Log.Println("Received new request:")
	Log.Println(r.Method, r.URL.Path)

	posLastSingleSlash := strings.LastIndex(r.URL.Path, "/")
	UrlLastPart := r.URL.Path[posLastSingleSlash+1:]

	// Remove trailing "/" if any
	if len(UrlLastPart) == 0 {
		path_without_trailing_slash := r.URL.Path[0:posLastSingleSlash]
		posLastSingleSlash = strings.LastIndex(path_without_trailing_slash, "/")
		UrlLastPart = path_without_trailing_slash[posLastSingleSlash+1:]
	}

	if strings.Contains(r.URL.Path, liveJobsEndpoint) {
		if !(r.Method == "POST" || r.Method == "GET" || r.Method == "PUT" || r.Method == "DELETE") {
			err := "Method = " + r.Method + " is not allowed to " + r.URL.Path
			Log.Println(err)
			http.Error(w, "405 method not allowed\n  Error: "+err, http.StatusMethodNotAllowed)
			return
		}

		if r.Method == "POST" && UrlLastPart != liveJobsEndpoint {
			res := "POST to " + r.URL.Path + "is not allowed"
			Log.Println(res)
			http.Error(w, "400 bad request\n  Error: "+res, http.StatusBadRequest)
		} else if r.Method == "POST" && UrlLastPart == liveJobsEndpoint {
			if r.Body == nil {
				res := "Error New live job without job specification"
				Log.Println("Error New live job without job specifications")
				http.Error(w, "400 bad request\n  Error: "+res, http.StatusBadRequest)
				return
			}

			var jspec job.LiveJobSpec
			e := json.NewDecoder(r.Body).Decode(&jspec)
			if e != nil {
				res := "Failed to decode job request"
				Log.Println("Error happened in JSON marshal. Err: ", e)
				http.Error(w, "400 bad request\n  Error: "+res, http.StatusBadRequest)
				return
			}

			//Log.Println("Header: ", r.Header)
			//Log.Printf("Job: %+v\n", job)

			err_validate, warnings := job.Validate(&jspec)
			if err_validate != nil {
				warning_message := "Warnings: "
				for i, e := range warnings {
					warning_message += strconv.Itoa(i) + ": " + e
				}

				res := "Error: "
				res += err_validate.Error() + ".  "
				res += warning_message
				http.Error(w, "400 bad request\n  "+res, http.StatusBadRequest)
				return
			}

			if jspec.Api_test == 1 {
				warning_message := "Warnings: "
				for _, e := range warnings {
					warning_message += e
				}

				w.Header().Set("Content-Type", "text/plain")
				fmt.Fprintln(w, warning_message)
				return
			}

			e1, j := createJob(jspec, warnings)
			if e1 != nil {
				http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
				return
			}

			b, e2 := json.Marshal(j)
			if e2 != nil {
				Log.Println("Failed to marshal new job to SQS message. Error: ", e2)
				http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
				return
			}

			// Send the create_job to job scheduler via SQS
			// create_job and stop_job share the same job queue.
			// A job "j" with "j.Stop" flag set to true indicates the job is to be stopped.
			// When "j" is added to the job queue and received by scheduler, the latter checks
			// the "j.Stop" flag to distinguish between a create_job and stop_job.
			jobMsg := string(b[:])
			e2 = sqs_sender.SendMsg(jobMsg, j.Id)
			if e2 != nil {
				Log.Println("Failed to send SQS message (New job). Error: ", e2)
				http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
				return
			}

			FileContentType := "application/json"
			w.Header().Set("Content-Type", FileContentType)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(j)
		} else if r.Method == "GET" {
			// Get all jobs: /jobs/
			if UrlLastPart == liveJobsEndpoint {
				FileContentType := "application/json"
				w.Header().Set("Content-Type", FileContentType)
				w.WriteHeader(http.StatusOK)

				jobs, err := getAllJobs()
				if err == nil {
					json.NewEncoder(w).Encode(jobs)
					return
				} else {
					http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
					return
				}
			} else if UrlLastPart == "active" {
				FileContentType := "application/json"
				w.Header().Set("Content-Type", FileContentType)
				w.WriteHeader(http.StatusOK)

				jobs, err := getAllActiveJobs()
				if err == nil {
					json.NewEncoder(w).Encode(jobs)
					return
				} else {
					http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
					return
				}
			} else { // Get one job: /jobs/[job_id]
				jid := UrlLastPart
				j, err := getJobById(jid)
				if err == nil {
					FileContentType := "application/json"
					w.Header().Set("Content-Type", FileContentType)
					w.WriteHeader(http.StatusOK)

					j.DrmEncryptionKeyInfo.Key = "********************************" // Hide DRM key
					json.NewEncoder(w).Encode(j)
					return
				} else {
					Log.Println("Non-existent job id: ", jid)
					http.Error(w, "Non-existent job id: "+jid, http.StatusNotFound)
					return
				}
			}
		} else if r.Method == "PUT" {
			if UrlLastPart == liveJobsEndpoint {
				res := "A job id must be provided when updating a job. "
				Log.Println(res, "Err: ", res)
				http.Error(w, "403 StatusForbidden\n  Error: "+res, http.StatusForbidden)
				return
			} else if strings.Contains(r.URL.Path, "stop") {
				begin := strings.Index(r.URL.Path, liveJobsEndpoint) + len(liveJobsEndpoint) + 1
				end := strings.Index(r.URL.Path, "stop") - 1
				jid := r.URL.Path[begin:end]
				Log.Println(jid)

				j, err := getJobById(jid)
				if err == nil {
					if j.State == job.JOB_STATE_STOPPED {
						res := "Trying to stop a already stopped job id: " + jid
						Log.Println(res)
						http.Error(w, "403 StatusForbidden\n  Error: "+res, http.StatusForbidden)
						return
					}

					w.WriteHeader(http.StatusAccepted)
					e1 := stopJob(j) // This function only updates Redis records
					j.Stop = true // Set Stop flag to true for the local variable

					if e1 != nil {
						http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
						return
					}

					b, e2 := json.Marshal(j)
					if e2 != nil {
						Log.Println("Failed to marshal stop_job to SQS message. Error: ", e2)
						http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
						return
					}

					// Send stop_job to job scheduler via SQS
					jobMsg := string(b[:])
					e2 = sqs_sender.SendMsg(jobMsg, j.Id)
					if e2 != nil {
						Log.Println("Failed to send SQS message (Stop job). Error: ", e2)
						http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
						return
					}

					return
				} else {
					res := "Cannot stop a non-existent job id = " + jid
					Log.Println(res)
					http.Error(w, "404 not found\n  Error: "+res, http.StatusNotFound)
					return
				}
			} else if strings.Contains(r.URL.Path, "resume") {
				begin := strings.Index(r.URL.Path, liveJobsEndpoint) + len(liveJobsEndpoint) + 1
				end := strings.Index(r.URL.Path, "resume") - 1
				jid := r.URL.Path[begin:end]
				Log.Println(jid)

				j, err := getJobById(jid)
				if err == nil {
					if j.State == job.JOB_STATE_RUNNING || j.State == job.JOB_STATE_STREAMING {
						res := "Trying to resume an active job id: " + jid
						Log.Println(res)
						http.Error(w, "403 StatusForbidden\n  Error: "+res, http.StatusForbidden)
						return
					}

					w.WriteHeader(http.StatusAccepted)
					e1 := resumeJob(j) // Update Redis
					j.Stop = false // Set Stop flag to false for the local variable

					if e1 != nil {
						http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
						return
					}

					b, e2 := json.Marshal(j)
					if e2 != nil {
						Log.Println("Failed to marshal resume_job to SQS message. Error: ", e2)
						http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
						return
					}

					// Send resume_job to job scheduler via SQS
					jobMsg := string(b[:])
					e2 = sqs_sender.SendMsg(jobMsg, j.Id)
					if e2 != nil {
						Log.Println("Failed to send SQS message (Stop job). Error: ", e2)
						http.Error(w, "500 internal server error\n  Error: ", http.StatusInternalServerError)
						return
					}

					return
				} else {
					res := "Trying to resume a non-existent job id: " + jid
					Log.Println(res)
					http.Error(w, "404 not found\n  Error: "+res, http.StatusNotFound)
					return
				}
			}
		} else if r.Method == "DELETE" {
			jid := UrlLastPart
			j, err := getJobById(jid)
			if err != nil {
				res := "Cannot delete a non-existent job id = " + jid
				Log.Println(res)
				http.Error(w, "404 not found\n  Error: "+res, http.StatusNotFound)
				return
			}

			if j.State != job.JOB_STATE_STOPPED {
				res := "Cannot delete a running job. Please stop the job first."
				Log.Println(res)
				http.Error(w, "403 forbidden\n  Error: "+res, http.StatusForbidden)
				return
			}

			err = deleteJob(j)
			if err == nil {
				FileContentType := "application/json"
				w.Header().Set("Content-Type", FileContentType)
				w.WriteHeader(http.StatusOK)

				res := "Successfully deleted job " + jid
				Log.Println(res)
				w.Write([]byte(res))
				return
			} else {
				res := "Failed to delete job " + jid + ". Error: " + err.Error()
				Log.Println(res)
				http.Error(w, res, http.StatusInternalServerError)
				return
			}
		}
	} else if strings.Contains(r.URL.Path, "feed") { // Demo ONLY!!!
		if !(r.Method == "POST" || r.Method == "DELETE") {
			err := "Method = " + r.Method + " is not allowed to " + r.URL.Path
			Log.Println(err)
			http.Error(w, "405 method not allowed\n  Error: "+err, http.StatusMethodNotAllowed)
			return
		}

		if r.Method == "POST" {
			if r.Body == nil {
				res := "Error: Trying to create live feed without input"
				Log.Println(res)
				http.Error(w, "400 bad request\n  Error: "+res, http.StatusBadRequest)
				return
			}

			var live_feed_spec demo.CreateLiveFeedSpec
			e := json.NewDecoder(r.Body).Decode(&live_feed_spec)
			if e != nil {
				res := "Failed to decode live feed spec"
				Log.Println("Error happened in JSON marshal (CreateLiveFeedSpec). Err: ", e)
				http.Error(w, "400 bad request\n  Error: "+res, http.StatusBadRequest)
				return
			}

			w.WriteHeader(http.StatusCreated)
			e = start_ffmpeg_live_contribution(live_feed_spec)
		} else if r.Method == "DELETE" {
			if !isLiveFeeding {
				res := "Live feeder isn't running. Cannot stop!"
				Log.Println(res)
				http.Error(w, "400 bad request\n  Error: "+res, http.StatusBadRequest)
			}

			stop_ffmpeg_live_contribution()
			w.WriteHeader(http.StatusCreated)
		}
	}
}

var server_hostname = "0.0.0.0"
var server_port = "1080"
var server_addr string
var Log *log.Logger
var server_config_file_path = "config.json"
var sqs_sender job_sqs.SqsSender
var redis redis_client.RedisClient
var server_config ApiServerConfig

func readConfig() {
	configFile, err := os.Open(server_config_file_path)
	if err != nil {
		fmt.Println(err)
	}

	defer configFile.Close()
	config_bytes, _ := ioutil.ReadAll(configFile)
	json.Unmarshal(config_bytes, &server_config)
}

func main() {
	configPtr := flag.String("config", "", "config file path")
	flag.Parse()

	if *configPtr != "" {
		server_config_file_path = *configPtr
	}

	readConfig()
	sqs_sender.QueueName = server_config.Sqs.Queue_name
	sqs_sender.SqsClient = sqs_sender.CreateClient()

	redis.RedisIp = server_config.Redis.RedisIp
	redis.RedisPort = server_config.Redis.RedisPort
	redis.Client, redis.Ctx = redis.CreateClient(redis.RedisIp, redis.RedisPort)

	// Cron will schedule logrotate events to automatically rotate all ezlivestreaming log files (logs that are under /home/streamer/logs/) every day.
	log_file_path := "/tmp/api_server.log"
	if server_config.Log_path != "" {
		posLastSingleSlash := strings.LastIndex(server_config.Log_path, "/")
		if posLastSingleSlash >= 0 {
			dir := server_config.Log_path[:posLastSingleSlash]
			_, err_fstat := os.Stat(dir)
			var err_mkdirall error
			if errors.Is(err_fstat, os.ErrNotExist) {
				err_mkdirall = os.MkdirAll(dir, 0750)
			}

			if err_mkdirall == nil {
				log_file_path = server_config.Log_path
			} else {
				fmt.Printf("Invalid log file path config (Failed to mkdirall). Use default path instead: %s\n", log_file_path)
			}
		} else {
			fmt.Printf("Invalid log file path config (bad path). Use default path instead: %s\n", log_file_path)
		}
	}

	var logfile, err1 = os.Create(log_file_path)
	if err1 != nil {
		panic(err1)
	}

	Log = log.New(logfile, "", log.LstdFlags)
	http.HandleFunc("/", main_server_handler)

	if server_config.Api_server_hostname != "" {
		server_hostname = server_config.Api_server_hostname
	}

	if server_config.Api_server_port != "" {
		server_port = server_config.Api_server_port
	}

	server_addr = server_hostname + ":" + server_port
	fmt.Println("API server listening on: ", server_addr)
	http.ListenAndServe(server_addr, nil)
}