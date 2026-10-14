package main

import "fmt"

// Quality levels: the user picks how close to the original a compressed copy should look,
// not an encoder setting.
//
// Photos: each level is a SSIMULACRA 2 score, mapped to an AVIF quality per kind of photo
// from measurements on real photos (5 iPhone HEICs, 6 JPEGs, 6 PNG screenshots; quality →
// mean score was consistent within a few points per kind). Videos: each level is a VMAF
// score, and the CRF is searched per video on short samples (videos vary too much for a
// table: the same CRF gave VMAF 94 to 99.9 on four iPhone clips).

type photoLevel struct {
	ID          string     `json:"id"`
	Label       string     `json:"label"`
	Description string     `json:"description"`
	Score       float64    `json:"ssimulacra2"` // target SSIMULACRA 2
	Qualities   QualitySet `json:"-"`
	// Typical size of the copy, % of the original, from the same measurements.
	TypicalSize QualitySet `json:"typical_size"`
}

var photoLevels = []photoLevel{
	{"lossless", "Visually lossless", "Can't be told apart even flipping between the two at full size.", 85,
		QualitySet{HEIC: 82, JPEG: 84, PNG: 90, Other: 84}, QualitySet{HEIC: 94, JPEG: 28, PNG: 16, Other: 28}},
	{"indistinguishable", "Indistinguishable side by side", "No visible difference next to the original at normal viewing.", 80,
		QualitySet{HEIC: 69, JPEG: 70, PNG: 66, Other: 70}, QualitySet{HEIC: 64, JPEG: 20, PNG: 9, Other: 20}},
	{"high", "High", "Differences only when zoomed in and compared.", 75,
		QualitySet{HEIC: 60, JPEG: 60, PNG: 49, Other: 60}, QualitySet{HEIC: 48, JPEG: 14, PNG: 7, Other: 14}},
	{"good", "Good", "Hard to notice without the original to compare.", 70,
		QualitySet{HEIC: 53, JPEG: 53, PNG: 40, Other: 53}, QualitySet{HEIC: 37, JPEG: 11, PNG: 5, Other: 11}},
	{"small", "Smallest", "Visible softening when zoomed in.", 60,
		QualitySet{HEIC: 43, JPEG: 44, PNG: 30, Other: 44}, QualitySet{HEIC: 25, JPEG: 8, PNG: 4, Other: 8}},
}

const defaultPhotoLevel = "indistinguishable"

type videoLevel struct {
	ID          string  `json:"id"`
	Label       string  `json:"label"`
	Description string  `json:"description"`
	VMAF        float64 `json:"vmaf"` // target mean VMAF (0: no search, see ByCamera)
	FallbackCRF int     `json:"-"`    // when ffmpeg has no VMAF: the typical CRF for it
	// ByCamera: a fixed CRF per kind of video (cameraCRF) instead of searching for the
	// VMAF target — much faster, chosen by eye on sample clips.
	ByCamera bool `json:"by_camera,omitempty"`
}

var videoLevels = []videoLevel{
	{"camera", "By camera (fast)", "A setting tuned for each camera, picked by eye: iPhone portrait CRF 44, iPhone landscape CRF 32, DJI CRF 38, others CRF 32. No quality search, so videos compress several times faster.", 0, 32, true},
	{"lossless", "Visually lossless", "Indistinguishable from the original (VMAF 98).", 98, 26, false},
	{"excellent", "Excellent", "No visible difference when watching normally (VMAF 96).", 96, 32, false},
	{"very-good", "Very good", "Rare, slight softness in fast or dark scenes (VMAF 94).", 94, 36, false},
	{"good", "Good", "Some softness visible when looking for it (VMAF 92).", 92, 40, false},
}

const defaultVideoLevel = "camera"

// cameraCRF is the "By camera" level's CRF for a video, and what kind it was taken for.
// Chosen on 2026-10-02 by watching 6-second clips of each kind at CRF 20–63.
func cameraCRF(p videoProbe) (int, string) {
	switch {
	case p.fromDJI():
		return 38, "DJI"
	case p.fromIPhone() && p.portrait():
		return 44, "iPhone portrait"
	case p.fromIPhone():
		return 32, "iPhone landscape"
	}
	return 32, "other camera"
}

func findPhotoLevel(id string) photoLevel {
	for _, l := range photoLevels {
		if l.ID == id {
			return l
		}
	}
	return findPhotoLevel(defaultPhotoLevel)
}

func findVideoLevel(id string) videoLevel {
	for _, l := range videoLevels {
		if l.ID == id {
			return l
		}
	}
	return findVideoLevel(defaultVideoLevel)
}

func validPhotoLevel(id string) bool { return findPhotoLevel(id).ID == id }
func validVideoLevel(id string) bool { return findVideoLevel(id).ID == id }

func (l photoLevel) String() string {
	return fmt.Sprintf("%s (SSIMULACRA 2 ≈ %.0f)", l.Label, l.Score)
}
func (l videoLevel) String() string { return fmt.Sprintf("%s (VMAF ≈ %.0f)", l.Label, l.VMAF) }

// minSaving: a compressed copy that saves less than this fraction is not kept (the
// original stays as it is): an already efficient HEIC or video can come out larger.
const minSaving = 0.10
