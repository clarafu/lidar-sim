# concourse lidar simulator

Runs the lidar component of [concourse](https://github.com/concourse/concourse) in a mocked out environment in order to create an isolated simulation. 

### How it works

Run the following command to start up the simulation using all default settings

```
go run main.go
```

The following flags can be passed in to simulate running lidar with different variables:
* `resource-checking-interval` = the interval that resources will be checked on, default is `1m`
* `lidar-runner-interval` = the interval that lidar will run the scanner and checker, default is `10s`
* `number-of-resources` = the number of resources that lidar will run checks for, default is `100` resources
* `check-duration` = the length of time that each resource check will take, default is `1s`
