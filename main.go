package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/glacier"
	"github.com/aws/aws-sdk-go-v2/service/glacier/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/manifoldco/promptui"
)

// For an AWS Account Cleanup the specified Vault
// 1. Check if the Vault is Empty
// 2. If Vault is Empty, Delete the Vault
// 3. If Vault is NOT Empty, Proceed to Empty the Vault
// 4. Check if there is an existing retrieval job
// 5. if no retrieval job exists initiate new retrieval job
// 6. Initiate a Retrieval Job for the Non Empty Vault
// 7. Wait for Retrieval Job to complete
// 8. Once Retrieval Job is complete iterate Job Output and delete the Archives one by one (we good add concurrency here but for a simple script it is not necessary)
func main() {
	fmt.Println("Global Warming is a CLI tool to delete your data from S3 Glacier Storage")

	cfg, err := config.LoadDefaultConfig(context.TODO())
	if err != nil {
		log.Fatal(err)
	}

	/*
		accountId := "971280941531"
	*/

	// TODO: add validation that AWS credentials match the account id provided
	// or we can just ask for credentials
	// I think we just throw an error saying credentials used are invalid. or maybe we prompt for credentials
	// first then we prompt for an account id and at that point we can check if we have correct access
	// in that account
	accountPrompt := promptui.Prompt{
		Label: "Enter AWS Account ID",
	}

	accountId, err := accountPrompt.Run()

	if err != nil {
		fmt.Printf("Prompt failed %v\n", err)
		return
	}

	if accountId == "" {

		stsClient := sts.NewFromConfig(cfg)
		output, err := stsClient.GetCallerIdentity(context.Background(), &sts.GetCallerIdentityInput{})
		if err != nil {
			log.Fatal(err)
		}

		accountId = *output.Account
	}

	fmt.Printf("You choose %q\n", accountId)

	regionList := GetRegionList(cfg)

	searcher := func(input string, index int) bool {
		region := regionList[index]
		val := strings.Replace(strings.ToLower(region), " ", "", -1)
		input = strings.Replace(strings.ToLower(input), " ", "", -1)

		return strings.Contains(val, input)
	}

	regionPrompt := promptui.Select{
		Label:    "Select the Region you want to empty",
		Items:    regionList,
		Searcher: searcher,
	}

	_, region, err := regionPrompt.Run()
	if err != nil {
		log.Fatal(err)
	}

	cfg, err = config.LoadDefaultConfig(context.TODO(), config.WithRegion(region))
	if err != nil {
		log.Fatal(err)
	}

	client := glacier.NewFromConfig(cfg)

	vaultPrompt := promptui.Select{
		Label: "Select the Vault you want to empty",
		Items: GetVaultList(client, accountId),
	}

	_, vaultName, err := vaultPrompt.Run()

	if err != nil {
		fmt.Printf("Prompt failed %v\n", err)
		return
	}

	fmt.Printf("You choose %q\n", vaultName)

	jobsPrompt := promptui.Select{
		Label: "Select a Job",
		Items: GetRetriavalJobs(client, accountId, vaultName),
	}

	_, jobId, err := jobsPrompt.Run()

	if err != nil {
		fmt.Printf("Prompt failed %v\n", err)
		return
	}

	switch jobId {
	case "Initiate New Retrieval Job":
		jobId, err = InitiateJob(client, accountId, vaultName)
		if err != nil {
			panic(err)
		}
	case "Exit":
		return
	default:
		if output, err := GetVaultRetrievalStatus(client, accountId, vaultName, jobId); err != nil {
			log.Fatal(err)
		} else {
			if output.Completed {
				log.Println("Vault Retrieval Complete")
				cleanupPrompt := promptui.Select{
					Label: "Empty Vault?",
					Items: []string{"Yes", "No"},
				}
				_, choice, err := cleanupPrompt.Run()
				if err != nil {
					log.Fatal(err)
				}
				switch choice {
				case "Yes":
					if err := EmptyVault(client, accountId, vaultName, jobId); err != nil {
						log.Fatal(err)
					}
					// InitiateJob(client, accountId, vaultName)
					return
				case "No":
					log.Println("Skipping cleanup and exiting")
					return
				default:
					log.Println("Invalid Choice")
					return
				}
			} else {
				log.Println("Vault Retrieval In Progress")
				return
			}
		}
	}
}

func GetRegionList(cfg aws.Config) []string {
	regionList := []string{}

	client := ec2.NewFromConfig(cfg)
	output, err := client.DescribeRegions(context.TODO(), &ec2.DescribeRegionsInput{})
	if err != nil {
		panic(err)
	}
	for _, region := range output.Regions {
		regionList = append(regionList, *region.RegionName)
	}
	return regionList
}

func GetVaultList(client *glacier.Client, accountId string) []string {
	output, err := client.ListVaults(context.Background(), &glacier.ListVaultsInput{
		AccountId: aws.String(accountId),
	})
	if err != nil {
		panic(err)
	}
	vaultList := []string{}
	for _, vault := range output.VaultList {
		vaultList = append(vaultList, *vault.VaultName)
	}
	if len(vaultList) == 0 {
		log.Fatalln("No Vaults Found try a different account or region")
	}
	return vaultList
}

func GetMostRecentJob(jobs []types.GlacierJobDescription) types.GlacierJobDescription {
	sort.Slice(jobs, func(i, j int) bool {
		return *jobs[i].CreationDate < *jobs[j].CreationDate
	})
	return jobs[0]
}

// GetRetrievalJob returns the job id of the most recent retrieval job
func GetRetriavalJobs(client *glacier.Client, accountId string, vaultName string) []string {

	jobs := []string{"Initiate New Retrieval Job"}

	output, err := client.ListJobs(context.Background(), &glacier.ListJobsInput{
		AccountId: aws.String(accountId),
		VaultName: aws.String(vaultName),
	})
	if err != nil {
		panic(err)
	}
	if len(output.JobList) == 0 {
		log.Printf("No Existing Jobs for vault: %s, in account: %s", vaultName, accountId)
	}
	sort.Slice(output.JobList, func(i, j int) bool {
		return *output.JobList[i].CreationDate < *output.JobList[j].CreationDate
	})
	for _, job := range output.JobList {
		jobs = append(jobs, *job.JobId)
	}
	jobs = append(jobs, "Exit")
	return jobs

}

func InitiateJob(client *glacier.Client, accountId string, vaultName string) (string, error) {
	output, err := client.InitiateJob(
		context.TODO(),
		&glacier.InitiateJobInput{
			AccountId:     aws.String(accountId),
			VaultName:     aws.String(vaultName),
			JobParameters: &types.JobParameters{Type: aws.String("inventory-retrieval")},
		})
	if err != nil {
		log.Printf("error initiating retrieval job: %+v", err)
		return "", err
	}
	log.Printf("Job Initialized: %s", *output.JobId)
	return *output.JobId, nil
}

func GetVaultRetrievalStatus(client *glacier.Client, accountId string, vaultName string, jobId string) (*glacier.DescribeJobOutput, error) {
	output, err := client.DescribeJob(context.Background(), &glacier.DescribeJobInput{
		AccountId: aws.String(accountId),
		JobId:     aws.String(jobId),
		VaultName: aws.String(vaultName),
	})
	if err != nil {
		log.Printf("error describing job: %+v", err)
		return nil, err
	}
	return output, nil
}

// Generated by https://quicktype.io
//
// To change quicktype's target language, run command:
//
//   "Set quicktype target language"

type VaultInventory struct {
	VaultARN      string        `json:"VaultARN"`
	InventoryDate string        `json:"InventoryDate"`
	ArchiveList   []ArchiveList `json:"ArchiveList"`
}

type ArchiveList struct {
	ArchiveID          string `json:"ArchiveId"`
	ArchiveDescription string `json:"ArchiveDescription"`
	CreationDate       string `json:"CreationDate"`
	Size               int64  `json:"Size"`
	SHA256TreeHash     string `json:"SHA256TreeHash"`
}

func EmptyVault(client *glacier.Client, accountId string, vaultName string, jobId string) error {
	output, err := GetVaultInventory(client, accountId, vaultName, jobId)
	if err != nil {
		log.Printf("error getting vault inventory: %+v", err)
		return err
	}
	if len(output.ArchiveList) == 0 {
		log.Printf("no archives to delete")
		deleteVaultPrompt := promptui.Select{
			Label: "Delete Vault?",
			Items: []string{"No", "Yes"},
		}
		_, choice, err := deleteVaultPrompt.Run()
		if err != nil {
			log.Printf("Prompt failed %v\n", err)
		}
		if choice == "Yes" {
			client.DeleteVault(context.TODO(), &glacier.DeleteVaultInput{VaultName: aws.String(vaultName)})
		}
	} else {
		log.Printf("%d archives to delete\n", len(output.ArchiveList))
		log.Printf("deleting archives\n")
		for _, archive := range output.ArchiveList {
			if err := DeleteArchive(client, accountId, vaultName, archive.ArchiveID); err != nil {
				log.Printf("error deleting archive: %+v", err)
			}
		}
	}

	return nil
}

func GetVaultInventory(client *glacier.Client, accountId string, vaultName string, jobId string) (*VaultInventory, error) {
	output, err := client.GetJobOutput(context.Background(), &glacier.GetJobOutputInput{
		AccountId: aws.String(accountId),
		JobId:     aws.String(jobId),
		VaultName: aws.String(vaultName),
	})

	if err != nil {
		log.Printf("error describing job: %+v", err)
	}
	body, _ := io.ReadAll(output.Body)
	inventory := VaultInventory{}
	if err := json.Unmarshal(body, &inventory); err != nil {
		return nil, err
	}
	return &inventory, nil
}

func DeleteArchive(client *glacier.Client, accountId string, vaultName string, archiveId string) error {
	log.Println("deleting archive", archiveId)
	_, err := client.DeleteArchive(context.Background(), &glacier.DeleteArchiveInput{
		AccountId: aws.String(accountId),
		VaultName: aws.String(vaultName),
		ArchiveId: aws.String(archiveId),
	})
	if err != nil {
		return err
	}
	return nil
}
