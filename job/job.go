package job

import (
	"ezliveStreaming/models"
	"time"
)

type LiveVideoOutputSpec struct {
	//video_output_label string `json:"label"`
	Codec       string
	Framerate   float64
	Width       int
	Height      int
	Bitrate     string
	Max_bitrate string
	Buf_size    string
	Preset      string
	Crf         int
	Threads     int
}

type LiveAudioOutputSpec struct {
	//audio_output_label string `json:"label"`
	Codec   string
	Bitrate string
}

type DrmConfig struct {
	Disable_clear_key int
	Protection_system string
	Protection_scheme string
}

type s3OutputConfig struct {
	Bucket string
}

type LiveJobOutputSpec struct {
	Stream_type             string
	Segment_format          string
	Segment_duration        int
	Fragment_duration       int
	Low_latency_mode        int
	Time_shift_buffer_depth int
	Drm                     DrmConfig
	S3_output               s3OutputConfig
	Video_outputs           []LiveVideoOutputSpec
	Audio_outputs           []LiveAudioOutputSpec
}

type LiveJobInputSpec struct {
	Url            string
	JobUdpPortBase int
}

type LiveJobSpec struct {
	Input  LiveJobInputSpec
	Output LiveJobOutputSpec
}

const JOB_STATE_CREATED = "created"     // Created
const JOB_STATE_RUNNING = "running"     // worker_transcoder running but not ingesting
const JOB_STATE_STREAMING = "streaming" // Ingesting and transcoding
const JOB_STATE_STOPPED = "stopped"     // Stopped

type LiveJob struct {
	Id    string
	State string
	// Job stats:
	Time_last_worker_report_ms  int64
	Ingress_bandwidth_kbps      int64  // reported by worker
	Transcoding_cpu_utilization string // reported by worker
	Total_bytes_ingested        int64  // total bytes ingested since the job was launched.
	Total_up_seconds            int64  // elapsed time since the job was launched/resumed.
	Total_active_seconds        int64  // elapsed time since the job becomes active (ingesting).
	// End job stats
	Playback_url               string
	RtmpIngestUrl              string
	RtmpIngestPort             int
	Input_info_url             string
	Spec                       LiveJobSpec
	Job_validation_warnings    string
	StreamKey                  string
	Time_created               time.Time
	Time_received_by_scheduler time.Time
	Time_received_by_worker    time.Time
	Assigned_worker_id         string
	DrmEncryptionKeyInfo       models.KeyInfo
	Stop                       bool // A flag indicating the job is to be stopped
	Delete                     bool // A flag indicating the job is to be deleted
}
