// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package consoleql_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/graphql-go/graphql"
	"github.com/skyrings/skyring-common/tools/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"storj.io/storj/internal/post"
	"storj.io/storj/internal/testcontext"
	"storj.io/storj/pkg/auth"
	"storj.io/storj/satellite"
	"storj.io/storj/satellite/console"
	"storj.io/storj/satellite/console/consoleauth"
	"storj.io/storj/satellite/console/consoleweb/consoleql"
	"storj.io/storj/satellite/mailservice"
	"storj.io/storj/satellite/payments/localpayments"
	"storj.io/storj/satellite/satellitedb/satellitedbtest"
)

// discardSender discard sending of an actual email
type discardSender struct{}

// SendEmail immediately returns with nil error
func (*discardSender) SendEmail(ctx context.Context, msg *post.Message) error {
	return nil
}

// FromAddress returns empty post.Address
func (*discardSender) FromAddress() post.Address {
	return post.Address{}
}

func TestGrapqhlMutation(t *testing.T) {
	satellitedbtest.Run(t, func(t *testing.T, db satellite.DB) {
		ctx := testcontext.New(t)
		defer ctx.Cleanup()

		log := zaptest.NewLogger(t)

		service, err := console.NewService(
			log,
			&consoleauth.Hmac{Secret: []byte("my-suppa-secret-key")},
			db.Console(),
			db.Rewards(),
			localpayments.NewService(nil),
			console.TestPasswordCost,
		)
		require.NoError(t, err)

		mailService, err := mailservice.New(log, &discardSender{}, "testdata")
		require.NoError(t, err)
		defer ctx.Check(mailService.Close)

		rootObject := make(map[string]interface{})
		rootObject["origin"] = "http://doesntmatter.com/"
		rootObject[consoleql.ActivationPath] = "?activationToken="
		rootObject[consoleql.SignInPath] = "login"

		schema, err := consoleql.CreateSchema(log, service, mailService)
		require.NoError(t, err)

		createUser := console.CreateUser{
			UserInfo: console.UserInfo{
				FullName:  "John Roll",
				ShortName: "Roll",
				Email:     "test@mail.test",
				PartnerID: "310bc643-684f-44b7-ac9f-3380373b45a1",
			},
			Password: "123a123",
		}
		refUserID := ""

		regToken, err := service.CreateRegToken(ctx, 1)
		require.NoError(t, err)

		rootUser, err := service.CreateUser(ctx, createUser, regToken.Secret, refUserID)
		require.NoError(t, err)
		require.Equal(t, createUser.PartnerID, rootUser.PartnerID.String())

		activationToken, err := service.GenerateActivationToken(ctx, rootUser.ID, rootUser.Email)
		require.NoError(t, err)

		err = service.ActivateAccount(ctx, activationToken)
		require.NoError(t, err)

		token, err := service.Token(ctx, createUser.Email, createUser.Password)
		require.NoError(t, err)

		sauth, err := service.Authorize(auth.WithAPIKey(ctx, []byte(token)))
		require.NoError(t, err)

		authCtx := console.WithAuth(ctx, sauth)

		t.Run("Create user mutation", func(t *testing.T) {
			newUser := console.CreateUser{
				UserInfo: console.UserInfo{
					FullName:  "Green Mickey",
					ShortName: "Green",
					Email:     "u1@mail.test",
					PartnerID: "e1b3e8a6-b9a2-4fd0-bb87-3ae87828264c",
				},
				Password: "123a123",
			}

			regTokenTest, err := service.CreateRegToken(ctx, 1)
			require.NoError(t, err)

			query := fmt.Sprintf(
				"mutation {createUser(input:{email:\"%s\",password:\"%s\", fullName:\"%s\", shortName:\"%s\", partnerId:\"%s\"}, secret: \"%s\"){id,shortName,fullName,email,partnerId,createdAt}}",
				newUser.Email,
				newUser.Password,
				newUser.FullName,
				newUser.ShortName,
				newUser.PartnerID,
				regTokenTest.Secret,
			)

			result := graphql.Do(graphql.Params{
				Schema:        schema,
				Context:       ctx,
				RequestString: query,
				RootObject:    rootObject,
			})

			for _, err := range result.Errors {
				assert.NoError(t, err)
			}
			require.False(t, result.HasErrors())

			data := result.Data.(map[string]interface{})
			usrData := data[consoleql.CreateUserMutation].(map[string]interface{})
			idStr := usrData["id"].(string)

			uID, err := uuid.Parse(idStr)
			assert.NoError(t, err)

			user, err := service.GetUser(authCtx, *uID)
			assert.NoError(t, err)

			assert.Equal(t, newUser.FullName, user.FullName)
			assert.Equal(t, newUser.ShortName, user.ShortName)
			assert.Equal(t, newUser.PartnerID, user.PartnerID.String())
		})

		testQuery := func(t *testing.T, query string) interface{} {
			result := graphql.Do(graphql.Params{
				Schema:        schema,
				Context:       authCtx,
				RequestString: query,
				RootObject:    rootObject,
			})

			for _, err := range result.Errors {
				assert.NoError(t, err)
			}
			require.False(t, result.HasErrors())

			return result.Data
		}

		t.Run("Update account mutation email only", func(t *testing.T) {
			email := "new@mail.test"
			query := fmt.Sprintf(
				"mutation {updateAccount(input:{email:\"%s\"}){id,email,fullName,shortName,createdAt}}",
				email,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			user := data[consoleql.UpdateAccountMutation].(map[string]interface{})

			assert.Equal(t, rootUser.ID.String(), user[consoleql.FieldID])
			assert.Equal(t, email, user[consoleql.FieldEmail])
			assert.Equal(t, rootUser.FullName, user[consoleql.FieldFullName])
			assert.Equal(t, rootUser.ShortName, user[consoleql.FieldShortName])
		})

		t.Run("Update account mutation fullName only", func(t *testing.T) {
			fullName := "George"
			query := fmt.Sprintf(
				"mutation {updateAccount(input:{fullName:\"%s\"}){id,email,fullName,shortName,createdAt}}",
				fullName,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			user := data[consoleql.UpdateAccountMutation].(map[string]interface{})

			assert.Equal(t, rootUser.ID.String(), user[consoleql.FieldID])
			assert.Equal(t, rootUser.Email, user[consoleql.FieldEmail])
			assert.Equal(t, fullName, user[consoleql.FieldFullName])
			assert.Equal(t, rootUser.ShortName, user[consoleql.FieldShortName])
		})

		t.Run("Update account mutation shortName only", func(t *testing.T) {
			shortName := "Yellow"
			query := fmt.Sprintf(
				"mutation {updateAccount(input:{shortName:\"%s\"}){id,email,fullName,shortName,createdAt}}",
				shortName,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			user := data[consoleql.UpdateAccountMutation].(map[string]interface{})

			assert.Equal(t, rootUser.ID.String(), user[consoleql.FieldID])
			assert.Equal(t, rootUser.Email, user[consoleql.FieldEmail])
			assert.Equal(t, rootUser.FullName, user[consoleql.FieldFullName])
			assert.Equal(t, shortName, user[consoleql.FieldShortName])
		})

		t.Run("Update account mutation all info", func(t *testing.T) {
			email := "test@newmail.com"
			fullName := "Fill Goal"
			shortName := "Goal"

			query := fmt.Sprintf(
				"mutation {updateAccount(input:{email:\"%s\",fullName:\"%s\",shortName:\"%s\"}){id,email,fullName,shortName,createdAt}}",
				email,
				fullName,
				shortName,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			user := data[consoleql.UpdateAccountMutation].(map[string]interface{})

			assert.Equal(t, rootUser.ID.String(), user[consoleql.FieldID])
			assert.Equal(t, email, user[consoleql.FieldEmail])
			assert.Equal(t, fullName, user[consoleql.FieldFullName])
			assert.Equal(t, shortName, user[consoleql.FieldShortName])

			createdAt := time.Time{}
			err := createdAt.UnmarshalText([]byte(user[consoleql.FieldCreatedAt].(string)))

			assert.NoError(t, err)
			assert.True(t, rootUser.CreatedAt.Equal(createdAt))
		})

		t.Run("Change password mutation", func(t *testing.T) {
			newPassword := "145a145a"

			query := fmt.Sprintf(
				"mutation {changePassword(password:\"%s\",newPassword:\"%s\"){id,email,fullName,shortName,createdAt}}",
				createUser.Password,
				newPassword,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			user := data[consoleql.ChangePasswordMutation].(map[string]interface{})

			assert.Equal(t, rootUser.ID.String(), user[consoleql.FieldID])
			assert.Equal(t, rootUser.Email, user[consoleql.FieldEmail])
			assert.Equal(t, rootUser.FullName, user[consoleql.FieldFullName])
			assert.Equal(t, rootUser.ShortName, user[consoleql.FieldShortName])

			createdAt := time.Time{}
			err := createdAt.UnmarshalText([]byte(user[consoleql.FieldCreatedAt].(string)))

			assert.NoError(t, err)
			assert.True(t, rootUser.CreatedAt.Equal(createdAt))

			oldHash := rootUser.PasswordHash

			rootUser, err = service.GetUser(authCtx, rootUser.ID)
			require.NoError(t, err)

			assert.False(t, bytes.Equal(oldHash, rootUser.PasswordHash))

			createUser.Password = newPassword
		})

		token, err = service.Token(ctx, rootUser.Email, createUser.Password)
		require.NoError(t, err)

		sauth, err = service.Authorize(auth.WithAPIKey(ctx, []byte(token)))
		require.NoError(t, err)

		authCtx = console.WithAuth(ctx, sauth)

		var projectID string
		t.Run("Create project mutation", func(t *testing.T) {
			projectInfo := console.ProjectInfo{
				Name:        "Project name",
				Description: "desc",
			}

			query := fmt.Sprintf(
				"mutation {createProject(input:{name:\"%s\",description:\"%s\"}){name,description,id,createdAt}}",
				projectInfo.Name,
				projectInfo.Description,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			project := data[consoleql.CreateProjectMutation].(map[string]interface{})

			assert.Equal(t, projectInfo.Name, project[consoleql.FieldName])
			assert.Equal(t, projectInfo.Description, project[consoleql.FieldDescription])

			projectID = project[consoleql.FieldID].(string)
		})

		pID, err := uuid.Parse(projectID)
		require.NoError(t, err)

		project, err := service.GetProject(authCtx, *pID)
		require.NoError(t, err)

		t.Run("Update project description mutation", func(t *testing.T) {
			query := fmt.Sprintf(
				"mutation {updateProjectDescription(id:\"%s\",description:\"%s\"){id,name,description}}",
				project.ID.String(),
				"",
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			proj := data[consoleql.UpdateProjectDescriptionMutation].(map[string]interface{})

			assert.Equal(t, project.ID.String(), proj[consoleql.FieldID])
			assert.Equal(t, project.Name, proj[consoleql.FieldName])
			assert.Equal(t, "", proj[consoleql.FieldDescription])
		})

		regTokenUser1, err := service.CreateRegToken(ctx, 1)
		require.NoError(t, err)

		user1, err := service.CreateUser(authCtx, console.CreateUser{
			UserInfo: console.UserInfo{
				FullName: "User1",
				Email:    "u1@mail.test",
			},
			Password: "123a123",
		}, regTokenUser1.Secret, refUserID)
		require.NoError(t, err)

		t.Run("Activation", func(t *testing.T) {
			activationToken1, err := service.GenerateActivationToken(
				ctx,
				user1.ID,
				"u1@mail.test",
			)
			require.NoError(t, err)

			err = service.ActivateAccount(ctx, activationToken1)
			require.NoError(t, err)

			user1.Email = "u1@mail.test"
		})

		regTokenUser2, err := service.CreateRegToken(ctx, 1)
		require.NoError(t, err)

		user2, err := service.CreateUser(authCtx, console.CreateUser{
			UserInfo: console.UserInfo{
				FullName: "User1",
				Email:    "u2@mail.test",
			},
			Password: "123a123",
		}, regTokenUser2.Secret, refUserID)
		require.NoError(t, err)

		t.Run("Activation", func(t *testing.T) {
			activationToken2, err := service.GenerateActivationToken(
				ctx,
				user2.ID,
				"u2@mail.test",
			)
			require.NoError(t, err)

			err = service.ActivateAccount(ctx, activationToken2)
			require.NoError(t, err)

			user2.Email = "u2@mail.test"
		})

		t.Run("Add project members mutation", func(t *testing.T) {
			query := fmt.Sprintf(
				"mutation {addProjectMembers(projectID:\"%s\",email:[\"%s\",\"%s\"]){id,name,members(limit:50,offset:0){joinedAt}}}",
				project.ID.String(),
				user1.Email,
				user2.Email,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			proj := data[consoleql.AddProjectMembersMutation].(map[string]interface{})

			assert.Equal(t, project.ID.String(), proj[consoleql.FieldID])
			assert.Equal(t, project.Name, proj[consoleql.FieldName])
			assert.Equal(t, 3, len(proj[consoleql.FieldMembers].([]interface{})))
		})

		t.Run("Delete project members mutation", func(t *testing.T) {
			query := fmt.Sprintf(
				"mutation {deleteProjectMembers(projectID:\"%s\",email:[\"%s\",\"%s\"]){id,name,members(limit:50,offset:0){user{id}}}}",
				project.ID.String(),
				user1.Email,
				user2.Email,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			proj := data[consoleql.DeleteProjectMembersMutation].(map[string]interface{})

			members := proj[consoleql.FieldMembers].([]interface{})
			rootMember := members[0].(map[string]interface{})[consoleql.UserType].(map[string]interface{})

			assert.Equal(t, project.ID.String(), proj[consoleql.FieldID])
			assert.Equal(t, project.Name, proj[consoleql.FieldName])
			assert.Equal(t, 1, len(members))

			assert.Equal(t, rootUser.ID.String(), rootMember[consoleql.FieldID])
		})

		var keyID string
		t.Run("Create api key mutation", func(t *testing.T) {
			keyName := "key1"
			query := fmt.Sprintf(
				"mutation {createAPIKey(projectID:\"%s\",name:\"%s\"){key,keyInfo{id,name,projectID,partnerId}}}",
				project.ID.String(),
				keyName,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			createAPIKey := data[consoleql.CreateAPIKeyMutation].(map[string]interface{})

			key := createAPIKey[consoleql.FieldKey].(string)
			keyInfo := createAPIKey[consoleql.APIKeyInfoType].(map[string]interface{})

			assert.NotEqual(t, "", key)

			assert.Equal(t, keyName, keyInfo[consoleql.FieldName])
			assert.Equal(t, project.ID.String(), keyInfo[consoleql.FieldProjectID])
			assert.Equal(t, rootUser.PartnerID.String(), keyInfo[consoleql.FieldPartnerID])

			keyID = keyInfo[consoleql.FieldID].(string)
		})

		t.Run("Delete api key mutation", func(t *testing.T) {
			id, err := uuid.Parse(keyID)
			require.NoError(t, err)

			info, err := service.GetAPIKeyInfo(authCtx, *id)
			require.NoError(t, err)

			query := fmt.Sprintf(
				"mutation {deleteAPIKeys(id:[\"%s\"]){name,projectID}}",
				keyID,
			)

			result := testQuery(t, query)
			data := result.(map[string]interface{})
			keyInfoList := data[consoleql.DeleteAPIKeysMutation].([]interface{})

			for _, k := range keyInfoList {
				keyInfo := k.(map[string]interface{})

				assert.Equal(t, info.Name, keyInfo[consoleql.FieldName])
				assert.Equal(t, project.ID.String(), keyInfo[consoleql.FieldProjectID])
			}
		})

		t.Run("Delete project mutation", func(t *testing.T) {
			query := fmt.Sprintf(
				"mutation {deleteProject(id:\"%s\"){id,name}}",
				projectID,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			proj := data[consoleql.DeleteProjectMutation].(map[string]interface{})

			assert.Equal(t, project.Name, proj[consoleql.FieldName])
			assert.Equal(t, project.ID.String(), proj[consoleql.FieldID])

			_, err := service.GetProject(authCtx, project.ID)
			assert.Error(t, err)
		})

		t.Run("Delete account mutation", func(t *testing.T) {
			query := fmt.Sprintf(
				"mutation {deleteAccount(password:\"%s\"){id}}",
				createUser.Password,
			)

			result := testQuery(t, query)

			data := result.(map[string]interface{})
			user := data[consoleql.DeleteAccountMutation].(map[string]interface{})

			assert.Equal(t, rootUser.ID.String(), user[consoleql.FieldID])

			_, err := service.GetUser(authCtx, rootUser.ID)
			assert.Error(t, err)
		})
	})
}
