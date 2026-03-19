package openstack

import (
	"fmt"
	"math/rand"
	"os"

	osclient "github.com/pfisterer/openstack-management-api/internal/openstack/client"
	"go.uber.org/zap"
)

func RunSomeTests(authURL, appCredID, appCredSecret, projectID, region string, insecure bool, log *zap.Logger, sugar *zap.SugaredLogger) {
	if log == nil {
		if sugar != nil {
			log = sugar.Desugar()
		} else {
			log = zap.NewNop()
		}
	}
	if sugar == nil {
		sugar = log.Sugar()
	}

	admin, err := osclient.NewOSAdminWithAppCredential(authURL, appCredID, appCredSecret, projectID, region, insecure, log, sugar)
	if err != nil {
		log.Error("Authentication failed", zap.Error(err))
		fmt.Fprintf(os.Stderr, "\nAuthentication failed. Possible causes:\n")
		fmt.Fprintf(os.Stderr, "  1. Application credentials have expired or are invalid\n")
		fmt.Fprintf(os.Stderr, "  2. Incorrect auth URL (should end with /v3)\n")
		fmt.Fprintf(os.Stderr, "  3. Network connectivity issues\n\n")
		fmt.Fprintf(os.Stderr, "To create new credentials:\n")
		fmt.Fprintf(os.Stderr, "  openstack application credential create backend-admin --unrestricted\n\n")
		os.Exit(1)
	}

	log.Info("Creating users")
	userNames := []string{"alice", "bob", "charlie", "diana", "eve"}
	createdUsers := make(map[string]string)

	memberRole, err := admin.FindRoleByName("member")
	if err != nil {
		log.Fatal("Failed to find member role", zap.Error(err))
	}

	for _, userName := range userNames {
		// Check if user already exists
		existingUser, err := admin.FindUserByName(userName)
		if err != nil {
			log.Warn("Failed to check for existing user", zap.String("user", userName), zap.Error(err))
			continue
		}

		if existingUser != nil {
			log.Info("User already exists, skipping creation", zap.String("user", userName), zap.String("id", existingUser.ID))
			createdUsers[userName] = existingUser.ID
			continue
		}

		user, err := admin.CreateUser(userName, "password123", userName+"@example.com", true)
		if err != nil {
			log.Warn("Failed to create user", zap.String("user", userName), zap.Error(err))
			continue
		}
		createdUsers[userName] = user.ID
	}

	log.Info("Creating groups")
	groupDefs := []struct {
		name        string
		description string
	}{
		{"developers", "Development team"},
		{"operations", "Operations team"},
		{"admins", "Administrative team"},
	}

	createdGroups := make(map[string]string)

	for _, gd := range groupDefs {
		// Check if group already exists
		existingGroup, err := admin.FindGroupByName(gd.name)
		if err != nil {
			log.Warn("Failed to check for existing group", zap.String("group", gd.name), zap.Error(err))
			continue
		}

		if existingGroup != nil {
			log.Info("Group already exists, skipping creation", zap.String("group", gd.name), zap.String("id", existingGroup.ID))
			createdGroups[gd.name] = existingGroup.ID
			continue
		}

		group, err := admin.CreateGroup(gd.name, gd.description)
		if err != nil {
			log.Warn("Failed to create group", zap.String("group", gd.name), zap.Error(err))
			continue
		}
		createdGroups[gd.name] = group.ID
	}

	log.Info("Adding users to groups randomly")
	groupNames := []string{"developers", "operations", "admins"}
	userNamesList := []string{"alice", "bob", "charlie", "diana", "eve"}

	for _, userName := range userNamesList {
		userID, ok := createdUsers[userName]
		if !ok {
			continue
		}

		// Randomly assign user to 1-2 groups
		numGroups := rand.Intn(2) + 1
		selectedGroups := make(map[string]bool)

		for i := 0; i < numGroups; i++ {
			groupName := groupNames[rand.Intn(len(groupNames))]
			if selectedGroups[groupName] {
				continue
			}
			selectedGroups[groupName] = true

			groupID, ok := createdGroups[groupName]
			if !ok {
				continue
			}

			if err := admin.AddUserToGroup(groupID, userID); err != nil {
				log.Warn("Failed to add user to group",
					zap.String("user", userName),
					zap.String("group", groupName),
					zap.Error(err))
			} else {
				log.Info("Added user to group",
					zap.String("user", userName),
					zap.String("group", groupName))
			}
		}
	}

	log.Info("Creating projects")
	projectDefs := []struct {
		name        string
		description string
	}{
		{"project-alpha", "Single user project for Alice"},
		{"project-beta", "Multi-user project for Bob and Charlie"},
		{"project-gamma", "Large team project"},
		{"project-delta", "Another multi-user project"},
	}

	createdProjects := make(map[string]string)

	for _, pd := range projectDefs {
		// Check if project already exists
		existingProject, err := admin.FindProjectByName(pd.name)
		if err != nil {
			log.Warn("Failed to check for existing project", zap.String("project", pd.name), zap.Error(err))
			continue
		}

		if existingProject != nil {
			log.Info("Project already exists, skipping creation", zap.String("project", pd.name), zap.String("id", existingProject.ID))
			createdProjects[pd.name] = existingProject.ID
			continue
		}

		project, err := admin.CreateProject(osclient.ProjectCreateOpts{
			BaseProjectOpts: osclient.BaseProjectOpts{
				Name:        pd.name,
				Description: &pd.description,
				DomainID:    "default",
				Enabled:     &[]bool{true}[0],
			},
		})
		if err != nil {
			log.Warn("Failed to create project", zap.String("project", pd.name), zap.Error(err))
			continue
		}
		createdProjects[pd.name] = project.ID
	}

	log.Info("Assigning users and groups to projects")

	// Project-alpha: Alice (user) + developers (group)
	if projID, ok := createdProjects["project-alpha"]; ok {
		// Assign Alice as user
		if aliceID, ok := createdUsers["alice"]; ok {
			if err := admin.AddProjectMember(projID, aliceID, memberRole.ID); err != nil {
				log.Warn("Failed to add member", zap.String("user", "alice"), zap.String("project", "project-alpha"), zap.Error(err))
			}
		}
		// Assign developers group
		if groupID, ok := createdGroups["developers"]; ok {
			if err := admin.AssignGroupToProject(projID, groupID, memberRole.ID); err != nil {
				log.Warn("Failed to assign group", zap.String("group", "developers"), zap.String("project", "project-alpha"), zap.Error(err))
			} else {
				log.Info("Assigned group to project", zap.String("group", "developers"), zap.String("project", "project-alpha"))
			}
		}
	}

	// Project-beta: Bob, Charlie (users) + operations (group)
	if projID, ok := createdProjects["project-beta"]; ok {
		for _, userName := range []string{"bob", "charlie"} {
			if userID, ok := createdUsers[userName]; ok {
				if err := admin.AddProjectMember(projID, userID, memberRole.ID); err != nil {
					log.Warn("Failed to add member", zap.String("user", userName), zap.String("project", "project-beta"), zap.Error(err))
				}
			}
		}
		// Assign operations group
		if groupID, ok := createdGroups["operations"]; ok {
			if err := admin.AssignGroupToProject(projID, groupID, memberRole.ID); err != nil {
				log.Warn("Failed to assign group", zap.String("group", "operations"), zap.String("project", "project-beta"), zap.Error(err))
			} else {
				log.Info("Assigned group to project", zap.String("group", "operations"), zap.String("project", "project-beta"))
			}
		}
	}

	// Project-gamma: Charlie, Diana, Eve (users) + developers + admins (groups)
	if projID, ok := createdProjects["project-gamma"]; ok {
		for _, userName := range []string{"charlie", "diana", "eve"} {
			if userID, ok := createdUsers[userName]; ok {
				if err := admin.AddProjectMember(projID, userID, memberRole.ID); err != nil {
					log.Warn("Failed to add member", zap.String("user", userName), zap.String("project", "project-gamma"), zap.Error(err))
				}
			}
		}
		// Assign multiple groups
		for _, groupName := range []string{"developers", "admins"} {
			if groupID, ok := createdGroups[groupName]; ok {
				if err := admin.AssignGroupToProject(projID, groupID, memberRole.ID); err != nil {
					log.Warn("Failed to assign group", zap.String("group", groupName), zap.String("project", "project-gamma"), zap.Error(err))
				} else {
					log.Info("Assigned group to project", zap.String("group", groupName), zap.String("project", "project-gamma"))
				}
			}
		}
	}

	// Project-delta: Bob (user) + all groups
	if projID, ok := createdProjects["project-delta"]; ok {
		if bobID, ok := createdUsers["bob"]; ok {
			if err := admin.AddProjectMember(projID, bobID, memberRole.ID); err != nil {
				log.Warn("Failed to add member", zap.String("user", "bob"), zap.String("project", "project-delta"), zap.Error(err))
			}
		}
		// Assign all groups
		for groupName, groupID := range createdGroups {
			if err := admin.AssignGroupToProject(projID, groupID, memberRole.ID); err != nil {
				log.Warn("Failed to assign group", zap.String("group", groupName), zap.String("project", "project-delta"), zap.Error(err))
			} else {
				log.Info("Assigned group to project", zap.String("group", groupName), zap.String("project", "project-delta"))
			}
		}
	}

	log.Info("Setting initial quotas")
	initialQuotas := osclient.QuotaSet{
		Instances:      2,
		Cores:          4,
		RAM:            8192,
		Networks:       2,
		Subnets:        4,
		Ports:          20,
		Routers:        1,
		FloatingIPs:    2,
		SecurityGroups: 5,
		Volumes:        5,
		Snapshots:      10,
		Gigabytes:      50,
	}

	for projName, projID := range createdProjects {
		initialQuotas.ProjectID = projID
		if err := admin.UpdateProjectQuotas(projID, initialQuotas); err != nil {
			log.Warn("Failed to set initial quotas", zap.String("project", projName), zap.Error(err))
		}
	}

	log.Info("Updating quotas with more capacity")
	updatedQuotas := osclient.QuotaSet{
		Instances:      10,
		Cores:          20,
		RAM:            40960,
		Networks:       5,
		Subnets:        10,
		Ports:          100,
		Routers:        5,
		FloatingIPs:    10,
		SecurityGroups: 20,
		Volumes:        20,
		Snapshots:      50,
		Gigabytes:      500,
	}

	for projName, projID := range createdProjects {
		updatedQuotas.ProjectID = projID
		if err := admin.UpdateProjectQuotas(projID, updatedQuotas); err != nil {
			log.Warn("Failed to update quotas", zap.String("project", projName), zap.Error(err))
		}
	}

	log.Info("Retrieving final state", zap.Int("project_count", len(createdProjects)))
	for projName, projID := range createdProjects {
		logger := log.With(zap.String("project", projName), zap.String("project_id", projID))

		members, err := admin.ListProjectMembers(projID)
		if err != nil {
			logger.Warn("Failed to list members", zap.Error(err))
		} else {
			userNames := make([]string, 0, len(members))
			for _, m := range members {
				for name, id := range createdUsers {
					if id == m.UserID {
						userNames = append(userNames, name)
						break
					}
				}
			}
			logger.Info("Project members", zap.Strings("members", userNames))
		}

		quotas, err := admin.GetProjectQuotas(projID)
		if err != nil {
			logger.Warn("Failed to get quotas", zap.Error(err))
		} else {
			logger.Info("Project quotas",
				zap.Int("instances", quotas.Instances),
				zap.Int("cores", quotas.Cores),
				zap.Int("ram_mb", quotas.RAM),
				zap.Int("volumes", quotas.Volumes),
				zap.Int("gigabytes", quotas.Gigabytes))
		}
	}

}
