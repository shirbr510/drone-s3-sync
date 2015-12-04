package main

import (
	"crypto/md5"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/ryanuber/go-glob"
)

type AWS struct {
	client *s3.S3
	remote []string
	local  []string
	vargs  PluginArgs
}

func NewAWS(vargs PluginArgs) AWS {
	sess := session.New(&aws.Config{
		Credentials: credentials.NewStaticCredentials(vargs.Key, vargs.Secret, ""),
		Region:      aws.String(vargs.Region),
	})
	c := s3.New(sess)
	r := make([]string, 1, 1)
	l := make([]string, 1, 1)

	return AWS{c, r, l, vargs}
}

func (a *AWS) visit(path string, info os.FileInfo, err error) error {
	if err != nil {
		return err
	}

	if path == "." {
		return nil
	}

	if info.IsDir() {
		return nil
	}

	localPath := strings.TrimPrefix(path, a.vargs.Source)
	if strings.HasPrefix(localPath, "/") {
		localPath = localPath[1:]
	}

	remotePath := filepath.Join(a.vargs.Target, localPath)

	a.local = append(a.local, localPath)
	file, err := os.Open(path)
	if err != nil {
		return err
	}

	defer file.Close()

	access := ""
	if a.vargs.Access.IsString() {
		access = a.vargs.Access.String()
	} else if !a.vargs.Access.IsEmpty() {
		accessMap := a.vargs.Access.Map()
		for pattern := range accessMap {
			if match := glob.Glob(pattern, localPath); match == true {
				access = accessMap[pattern]
				break
			}
		}
	}

	if access == "" {
		access = "private"
	}

	fileExt := filepath.Ext(localPath)
	var contentType string
	if a.vargs.ContentType.IsString() {
		contentType = a.vargs.ContentType.String()
	} else if !a.vargs.ContentType.IsEmpty() {
		contentMap := a.vargs.ContentType.Map()
		for patternExt := range contentMap {
			if patternExt == fileExt {
				contentType = contentMap[patternExt]
				break
			}
		}
	}

	metadata := map[string]*string{}
	vmap := a.vargs.Metadata.Map()
	if len(vmap) > 0 {
		for pattern := range vmap {
			if match := glob.Glob(pattern, localPath); match == true {
				for k, v := range vmap[pattern] {
					metadata[k] = aws.String(v)
				}
				break
			}
		}
	}

	if contentType == "" {
		contentType = mime.TypeByExtension(fileExt)
	}

	exists := false
	for _, remoteFile := range a.remote {
		if remoteFile == localPath {
			exists = true
			break
		}
	}

	if exists {
		hash := md5.New()
		io.Copy(hash, file)
		sum := fmt.Sprintf("\"%x\"", hash.Sum(nil))

		head, err := a.client.HeadObject(&s3.HeadObjectInput{
			Bucket: aws.String(a.vargs.Bucket),
			Key:    aws.String(remotePath),
		})
		if err != nil {
			return err
		}

		if sum == *head.ETag {
			shouldCopy := false

			if head.ContentType == nil && contentType != "" {
				debug("Content-Type has changed from unset to %s\n", contentType)
				shouldCopy = true
			}

			if !shouldCopy && head.ContentType != nil && contentType != *head.ContentType {
				debug("Content-Type has changed from %s to %s\n", *head.ContentType, contentType)
				shouldCopy = true
			}

			if !shouldCopy && len(head.Metadata) != len(metadata) {
				debug("Count of metadata values has changed for %s\n", localPath)
				shouldCopy = true
			}

			if !shouldCopy && len(metadata) > 0 {
				for k, v := range metadata {
					if hv, ok := head.Metadata[k]; ok {
						if *v != *hv {
							debug("Metadata values have changed for %s\n", localPath)
							shouldCopy = true
							break
						}
					}
				}
			}

			if !shouldCopy {
				grant, err := a.client.GetObjectAcl(&s3.GetObjectAclInput{
					Bucket: aws.String(a.vargs.Bucket),
					Key:    aws.String(remotePath),
				})
				if err != nil {
					return err
				}

				previousAccess := "private"
				for _, g := range grant.Grants {
					gt := *g.Grantee
					if gt.URI != nil {
						if *gt.URI == "http://acs.amazonaws.com/groups/global/AllUsers" {
							if *g.Permission == "READ" {
								previousAccess = "public-read"
							} else if *g.Permission == "WRITE" {
								previousAccess = "public-read-write"
							}
						} else if *gt.URI == "http://acs.amazonaws.com/groups/global/AllUsers" {
							if *g.Permission == "READ" {
								previousAccess = "authenticated-read"
							}
						}
					}
				}

				if previousAccess != access {
					debug("Permissions for \"%s\" have changed from \"%s\" to \"%s\"\n", remotePath, previousAccess, access)
					shouldCopy = true
				}
			}

			if !shouldCopy {
				debug("Skipping \"%s\" because hashes and metadata match\n", localPath)
				return nil
			}

			fmt.Printf("Updating metadata for \"%s\" Content-Type: \"%s\", ACL: \"%s\"\n", localPath, contentType, access)
			_, err = a.client.CopyObject(&s3.CopyObjectInput{
				Bucket:            aws.String(a.vargs.Bucket),
				Key:               aws.String(remotePath),
				CopySource:        aws.String(fmt.Sprintf("%s/%s", a.vargs.Bucket, remotePath)),
				ACL:               aws.String(access),
				ContentType:       aws.String(contentType),
				Metadata:          metadata,
				MetadataDirective: aws.String("REPLACE"),
			})
			return err
		}

		_, err = file.Seek(0, 0)
		if err != nil {
			return err
		}
	}

	fmt.Printf("Uploading \"%s\" with Content-Type \"%s\" and permissions \"%s\"\n", localPath, contentType, access)
	_, err = a.client.PutObject(&s3.PutObjectInput{
		Bucket:      aws.String(a.vargs.Bucket),
		Key:         aws.String(remotePath),
		Body:        file,
		ContentType: aws.String(contentType),
		ACL:         aws.String(access),
		Metadata:    metadata,
	})
	return err
}

func (a *AWS) AddRedirects(redirects map[string]string) error {
	for path, location := range redirects {
		fmt.Printf("Adding redirect from \"%s\" to \"%s\"", path, location)
		a.local = append(a.local, strings.TrimPrefix(path, "/"))
		_, err := a.client.PutObject(&s3.PutObjectInput{
			Bucket: aws.String(a.vargs.Bucket),
			Key:    aws.String(path),
			ACL:    aws.String("public-read"),
			WebsiteRedirectLocation: aws.String(location),
		})

		if err != nil {
			return err
		}
	}

	return nil
}

func (a *AWS) List(path string) error {
	resp, err := a.client.ListObjects(&s3.ListObjectsInput{
		Bucket: aws.String(a.vargs.Bucket),
		Prefix: aws.String(path),
	})
	if err != nil {
		return err
	}

	for _, item := range resp.Contents {
		a.remote = append(a.remote, *item.Key)
	}

	for *resp.IsTruncated {
		resp, err = a.client.ListObjects(&s3.ListObjectsInput{
			Bucket: aws.String(a.vargs.Bucket),
			Prefix: aws.String(path),
			Marker: aws.String(a.remote[len(a.remote)-1]),
		})

		if err != nil {
			return err
		}

		for _, item := range resp.Contents {
			a.remote = append(a.remote, *item.Key)
		}
	}

	return nil
}

func (a *AWS) Cleanup() error {
	for _, remote := range a.remote {
		found := false
		for _, local := range a.local {
			if local == remote {
				found = true
				break
			}
		}

		if !found {
			fmt.Printf("Removing remote file \"%s\"\n", remote)
			_, err := a.client.DeleteObject(&s3.DeleteObjectInput{
				Bucket: aws.String(a.vargs.Bucket),
				Key:    aws.String(remote),
			})
			if err != nil {
				return err
			}
		}
	}

	return nil
}
