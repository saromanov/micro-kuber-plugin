# Kuber-plugin
_Go Micro plugin for Kubernetes' registry support_

Kuber-Plugin is the Micro plugin that wraps the Kubernetes services into a
Micro plugins.

All services gave is in the form of:
1. service.Name with the Kubernetes' service name.
2. service.Nodes with one element in the slice: ClusterIP + first port.

If you ever need help or precisions about a method, read the code.

#### Automatic way — Gitlab CI deploy
You just have to import the package, the deployment system will take care of the
rest.
Importing it once in the main file is enough.

```go
import (
  _ "gitlab.contetto.io/contetto-micro/kuber-plugin"
)
```
_The trailing underscore is to not have warning for importing something without
using any property or object from it._

#### Manual way
Import it like in **Automatic way** and add the --registry=kubernetes argument
when launching the app.

It won't work locally, obviously.

#### Does it prevent me to use it locally?
No, without the --registry=kubernetes argument, it will use consul. So just
install consul locally for testing your microservice.
