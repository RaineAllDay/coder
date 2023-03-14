<!-- DO NOT EDIT | GENERATED CONTENT -->

# override-stop

Edit stop time of active workspace

## Usage

```console
override-stop <workspace-name> <duration from now>
```

## Description

```console
Override the stop time of a currently running workspace instance.
  * The new stop time is calculated from *now*.
  * The new stop time must be at least 30 minutes in the future.
  * The workspace template may restrict the maximum workspace runtime.

  $ coder schedule override-stop my-workspace 90m
```