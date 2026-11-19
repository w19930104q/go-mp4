package trim

import (
	"fmt"
	"math"
	"os"
	"strconv"

	"github.com/abema/go-mp4"
)

func Main(args []string) int {
	if len(args) < 4 {
		println("USAGE: mp4tool trim INPUT.mp4 OUTPUT.mp4 START_TIME END_TIME")
		println("  Extract the samples within the half-open time range [START_TIME, END_TIME)")
		println("  in seconds (e.g. 1.5) and write them to OUTPUT.mp4.")
		return 1
	}

	start, err := strconv.ParseFloat(args[2], 64)
	if err != nil {
		fmt.Println("Error: invalid START_TIME:", err)
		return 1
	}
	end, err := strconv.ParseFloat(args[3], 64)
	if err != nil {
		fmt.Println("Error: invalid END_TIME:", err)
		return 1
	}
	if start < 0 || end < 0 {
		fmt.Println("Error: START_TIME and END_TIME must not be negative")
		return 1
	}

	if err := trim(args[0], args[1], start, end); err != nil {
		fmt.Println("Error:", err)
		return 1
	}
	return 0
}

func trim(inputFilePath, outputFilePath string, start, end float64) error {
	inputFile, err := os.Open(inputFilePath)
	if err != nil {
		return err
	}
	defer inputFile.Close()

	// get the movie timescale from the mvhd box
	bis, err := mp4.ExtractBoxWithPayload(inputFile, nil, mp4.BoxPath{mp4.BoxTypeMoov(), mp4.BoxTypeMvhd()})
	if err != nil {
		return err
	}
	if len(bis) != 1 {
		return fmt.Errorf("mvhd box not found")
	}
	mvhd := bis[0].Payload.(*mp4.Mvhd)
	if mvhd.Timescale == 0 {
		return fmt.Errorf("mvhd timescale is zero")
	}
	timescale := float64(mvhd.Timescale)

	outputFile, err := os.Create(outputFilePath)
	if err != nil {
		return err
	}
	defer outputFile.Close()

	startTime := uint64(math.Round(start * timescale))
	endTime := uint64(math.Round(end * timescale))
	return mp4.ExtractRange(inputFile, outputFile, startTime, endTime)
}
