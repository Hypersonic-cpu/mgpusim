# MGPU-Sim LATPC Develop Notes

## MGPU-Sim Framework
### Building
```
go build .  in amd/samples/atax
│
├─ compile package main: amd/samples/atax/main.go
│
├─ compile imported benchmark package:
│    amd/benchmarks/polybench/atax
│    ├─ Go benchmark control code
│    ├─ embedded kernels.hsaco
│    └─ embedded kernels_gfx942.hsaco
│
├─ compile imported runner/platform packages:
│    amd/samples/runner
│    ├─ flag parsing
│    ├─ emulation platform builder
│    └─ timing platform builder
│
├─ compile simulator packages:
│    amd/driver
│    amd/timing/cp
│    amd/timing/cu      # ComputeUnit, Scheduler etc.
│    amd/timing/gmmu    # Translation
│    amd/timing/latpc   # LATPC extension
│    amd/protocol, amd/insts, amd/emu, ...
│
└─ link one executable:
     ./atax
```
LATPC is controlled by -translation-mode. The branch defines:
- baseline -> detailed walk only
- latc     -> detector + L1 TLB MSHR compression + detailed walk
- latp     -> detector + grouped page-table walks + detailed walk
- latpc    -> detector + LATC + LATP + detailed walk
- ideal    -> bypass / idealized translation path
- [ ] FIXME: Ideal results in lower speed-up.

### Sim Launch
```
main()
│
├─ flag.Parse()
│
├─ runner.Init()
│   ├─ parse simulator flags
│   ├─ create Akita simulation engine
│   ├─ build emulation platform        if no -timing
│   ├─ build detailed timing platform  if -timing
│   └─ optionally create unified GPU
│
├─ benchmark.Run()
│   ├─ load embedded HSACO kernel code objects
│   ├─ create driver context and command queues
│   ├─ allocate GPU / unified memory
│   ├─ MemCopyH2D
│   ├─ LaunchKernel
│   ├─ LaunchKernel
│   └─ MemCopyD2H
│
└─ runner cleanup
    ├─ wait for event engine idle
    ├─ report metrics
    ├─ terminate driver
    └─ terminate simulation
```

### Main Simulation Loop
```
ComputeUnit.Tick()
│
├─ runPipeline()
│   ├─ BranchUnit
│   ├─ ScalarUnit
│   ├─ ScalarDecoder
│   ├─ SIMD units
│   ├─ VectorDecoder
│   ├─ LDSUnit
│   ├─ LDSDecoder
│   ├─ VectorMemUnit
│   ├─ VectorMemDecoder
│   └─ Scheduler
│
├─ sendToCP()
├─ processInput()
└─ doFlush()
```

## LATPC Extension
Added components:
```
    gmmu               
  │ │  component.go     
  │ │  component_test.go
  │ │  pwc.go        
  │ │  walker.go     
  │ └  walker_test.go
    latpc             
  │ │  config.go       
  │ │  config_test.go  
  │ │  detector.go     
  │ │  detector_test.go
  │ │  latc.go     
  │ │  latc_test.go    
  │ │  latp.go         
  │ │  latp_test.go    
  │ │  metadata.go     
  │ └  metadata_test.go 
```

```
main.go
  │
  ▼
Runner
  │
  ▼
BFS Benchmark
  │
  ├─ loadProgram()
  │    └─ embed HSACO → KernelCodeObject("BFS_kernel_warp")
  │
  ├─ initMem()
  │    ├─ host graph arrays
  │    └─ simulated GPU virtual memory allocation
  │
  └─ exec()
       │
       ├─ MemCopyH2D
       │    └─ Driver → CommandQueue → CP → DMA → simulated memory
       │
       ├─ LaunchKernel
       │    └─ Driver
       │        → LaunchKernelCommand
       │        → LaunchKernelReq
       │        → GPU Command Processor
       │        → Dispatcher
       │        → algorithm.Next()
       │        → MapWGReq
       │        → CU
       │
       └─ MemCopyD2H
            └─ Driver → CommandQueue → CP → DMA → host Go object
```
