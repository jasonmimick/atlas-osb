name: full-plan
description: This is a full plan for a project, cluster, database user, and secure connection.
free: true
apiKey: {{ json (index .Credentials.Projects .Project.ID) }}
project:
  id: {{ .Project.ID }}
  name: {{ .Project.Name }}
  orgId: {{ .Project.OrgID }}
cluster:
  providerSettings:
    instanceSizeName: M20
databaseUsers:
- username: "test-user"
  password: "test-password"
  databaseName: "admin"
  groupId: {{ .Project.ID }}
  roles:
  - roleName: "readWrite"
    databaseName: "admin"
ipWhitelists:
- ipAddress: "0.0.0.0/1"
  comment: "everything"
  groupId: {{ .Project.ID }}
- ipAddress: "128.0.0.0/1"
  comment: "everything"
  groupId: {{ .Project.ID }}