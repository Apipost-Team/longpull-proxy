package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"reflect"
	"runtime"
	"strings"
	"time"
)

var transport = &http.Transport{
	MaxIdleConns:        10,               // 最大空闲连接数
	MaxIdleConnsPerHost: 10,               // 每个主机的最大空闲连接数
	IdleConnTimeout:     30 * time.Second, // 空闲连接的超时时间
}
var (
	//blockMap    sync.Map //不用使用线程安全
	blockMap = make(map[string]chan<- struct{})
	client   = &http.Client{
		Transport: transport,
	}
	backendBase string
)

type StatusResponse struct {
	Goroutines int `json:"goroutines"`
	BlockCount int `json:"block_count"`
}

func main() {
	// 通过命令行参数指定运行端口和后端地址
	port := flag.Int("port", 8080, "The port number to run the proxy server")
	backend := flag.String("backend", "", "The backend server URL")
	debug := flag.Int("debug", 0, "show debug log")
	flag.Parse()

	if *debug > 0 {
		log.SetOutput(os.Stdout)
	} else {
		log.SetOutput(io.Discard)
		log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	}

	// 验证命令行参数
	if *backend == "" {
		fmt.Println("Please provide the backend server URL")
		os.Exit(1)
	}
	backendBase = strings.TrimRight(*backend, "/") //去除末尾反斜杠

	//http.HandleFunc("/", helloHandler)
	http.HandleFunc("/cancel", cancelBlockHandler) //取消阻塞
	http.HandleFunc("/status", statusHandler)      //状态
	http.HandleFunc("/", proxyHandler)             //请求路径

	addr := fmt.Sprintf(":%d", *port)
	fmt.Printf("Proxy server is running on port %s...\nbackend %s\ndebug %d\n", addr, backendBase, *debug)
	fmt.Println(http.ListenAndServe(addr, nil))
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	// 读取请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Println("Failed to read request body:", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	// 配置后端服务器地址
	backendURL := backendBase + r.URL.Path
	log.Printf("%s %s", r.Method, backendURL)

	// 创建代理请求
	proxyReq, err := http.NewRequest(r.Method, backendURL, strings.NewReader(string(body)))
	if err != nil {
		log.Println("Failed to create proxy request:", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	// 复制请求头
	copyHeaders(proxyReq.Header, r.Header)

	// 发送代理请求
	resp, err := client.Do(proxyReq)
	if err != nil {
		log.Println("Failed to send proxy request:", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}
	defer resp.Body.Close()

	//检查是否需要block
	blockHeader := strings.TrimSpace(resp.Header.Get("x-block"))
	if blockHeader == "" {
		//无需block，直接将原始数据返回
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("X-Block-St", "pass") //提示前端没有阻塞
		w.Header().Set("X-Block-St-Time", time.Since(start).String())

		// 将响应返回给客户端
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Println("Failed to write response:", err)
		}
		return
	}

	// 解析阻塞时间
	var blockTime int
	_, err = fmt.Sscanf(blockHeader, "%ds", &blockTime)
	if err != nil {
		log.Println("Failed to parse block time:", err)
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(err.Error()))
		return
	}

	cancelCh := make(chan struct{})

	//get header etag
	etagHeader := strings.TrimSpace(resp.Header.Get("etag"))
	if etagHeader != "" {
		//通过etag可以取消阻塞
		blockMap[etagHeader] = cancelCh //存储阻塞chanel

		//同一个chanel再删除
		defer func() {
			if cancelCh2, ok := blockMap[etagHeader]; ok {
				if isSameChannel(cancelCh, cancelCh2) {
					delete(blockMap, etagHeader)
				} else {
					log.Printf("%s is dup", etagHeader)
				}
			}
		}()
	}

	var isCancel bool //确认是否被取消

	//阻塞
	log.Printf("%s %s %ds %s", r.Method, r.URL.Path, blockTime, etagHeader)
	select {
	case <-time.After(time.Duration(blockTime) * time.Second):
		isCancel = false
	case <-cancelCh:
		log.Printf("%s %s is cancel", r.Method, r.URL.Path)
		isCancel = true
	}

	if isCancel {
		// 收到取消阻塞的信号，从新到后端获取最新结果
		newResp, err := client.Do(proxyReq)
		if err != nil {
			log.Println("Failed to fetch updated result:", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		defer newResp.Body.Close()

		copyHeaders(w.Header(), newResp.Header)
		w.Header().Set("X-Block-St", "cancel") //提示前端被取消阻塞
		w.Header().Set("X-Block-St-Time", time.Since(start).String())
		w.WriteHeader(newResp.StatusCode)
		if _, err := io.Copy(w, newResp.Body); err != nil {
			log.Println("Failed to write response:", err)
		}
	} else {
		// 阻塞时间到达后继续执行
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("X-Block-St", "timeout") //提示前端超时
		w.Header().Set("X-Block-St-Time", time.Since(start).String())
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(w, resp.Body); err != nil {
			log.Println("Failed to write response:", err)
		}
	}
}

func cancelBlockHandler(w http.ResponseWriter, r *http.Request) {
	var etag string
	err := r.ParseForm()
	if err == nil {
		etag = r.FormValue("etag")
	}

	//获取etag
	if etag == "" {
		etag = r.URL.Query().Get("etag")
	}

	if etag == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"code":1, "msg":"etag is require"}`))
		return
	}

	cancelCh, ok := blockMap[etag]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"code":2, "msg":"etag is not found"}`))
		return
	}

	delete(blockMap, etag) //主动删除map

	//线程不安全，小心重复关闭
	select {
	case cancelCh <- struct{}{}:
		log.Printf("tag:%s cancelCh is send", etag)
	default:
		log.Printf("cancelCh is closed")
	}

	//返回json，json中提示 cancelok
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"code":0, "msg":"sucess","cancelok":true}`))
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	goroutines := runtime.NumGoroutine()
	status := StatusResponse{
		Goroutines: goroutines,
		BlockCount: len(blockMap),
	}
	responseJSON, err := json.Marshal(status)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write(responseJSON)
}

func copyHeaders(dest, src http.Header) {
	for key, values := range src {
		for _, value := range values {
			dest.Add(key, value)
		}
	}
}

// 判断两个 chan 是否是同一个
func isSameChannel(ch1, ch2 interface{}) bool {
	// 将 chan 的指针转换为 reflect.Value
	v1 := reflect.ValueOf(ch1)
	v2 := reflect.ValueOf(ch2)

	// 获取指针的地址
	p1 := v1.Pointer()
	p2 := v2.Pointer()

	// 比较两个指针的地址是否相同
	return p1 == p2
}
