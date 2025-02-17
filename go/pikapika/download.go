package pikapika

import (
	"bytes"
	"fmt"
	"image"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	comic_center2 "pikapika/pikapika/database/comic_center"
	utils2 "pikapika/pikapika/utils"
	"sync"
	"time"
)

// 使用协程进行后台下载
// downloadRunning 如果为false则停止下载
// downloadRestart 为true则取消从新启动下载功能

var downloadThreadCount = 1
var downloadThreadFetch = 100

var downloadRunning = false
var downloadRestart = false

var downloadingComic *comic_center2.ComicDownload
var downloadingEp *comic_center2.ComicDownloadEp

var dlFlag = true

// 程序启动后仅调用一次, 启动后台线程
func downloadBackground() {
	println("后台线程启动")
	if dlFlag {
		dlFlag = false
		go downloadBegin()
	}
}

// 下载启动/重新启动会暂停三秒
func downloadBegin() {
	time.Sleep(time.Second * 3)
	go downloadLoadComic()
}

// 下载周期中, 每个下载单元会调用此方法, 如果返回true应该停止当前动作
func downloadHasStop() bool {
	if !downloadRunning {
		return true
	}
	if downloadRestart {
		downloadRestart = false
		return true
	}
	return false
}

// 删除下载任务, 当用户要删除下载的时候, 他会被加入删除队列, 而不是直接被删除, 以减少出错
func downloadDelete() bool {
	c, e := comic_center2.DeletingComic()
	if e != nil {
		panic(e)
	}
	if c != nil {
		os.RemoveAll(downloadPath(c.ID))
		e = comic_center2.TrueDelete(c.ID)
		if e != nil {
			panic(e)
		}
		return true
	}
	return false
}

// 加载第一个需要下载的漫画
func downloadLoadComic() {
	// 每次下载完一个漫画, 或者启动的时候, 首先进行删除任务
	for downloadDelete() {
	}
	// 检测是否需要停止
	if downloadHasStop() {
		go downloadBegin()
		return
	}
	// 找到第一个要下载的漫画, 查库有错误就停止, 因为这些错误很少出现, 一旦出现必然是严重的, 例如数据库文件突然被删除
	var err error
	downloadingComic, err = comic_center2.LoadFirstNeedDownload()
	if err != nil {
		panic(err)
	}
	// 处理找到的下载任务
	go downloadInitComic()
}

// 初始化找到的下载任务
func downloadInitComic() {
	// 检测是否需要停止
	if downloadHasStop() {
		go downloadBegin()
		return
	}
	// 若没有漫画要下载则重新启动
	if downloadingComic == nil {
		println("没有找到要下载的漫画")
		go downloadBegin()
		return
	}
	// 打印日志, 并向前端的eventChannel发送下载信息
	println("正在下载漫画 " + downloadingComic.Title)
	downloadComicEventSend(downloadingComic)
	eps, err := comic_center2.ListDownloadEpByComicId(downloadingComic.ID)
	if err != nil {
		panic(err)
	}
	// 找到这个漫画需要下载的EP, 并搜索获取图片地址
	for _, ep := range eps {
		// FetchedPictures字段标志着这个章节的图片地址有没有获取过, 如果没有获取过就重新获取
		if !ep.FetchedPictures {
			println("正在获取章节的图片 " + downloadingComic.Title + " " + ep.Title)
			// 搜索图片地址, 如果五次没有请求成功, 就不在请求
			for i := 0; i < 5; i++ {
				if client.Token == "" {
					continue
				}
				err := downloadFetchPictures(&ep)
				if err != nil {
					println(err.Error())
					continue
				}
				ep.FetchedPictures = true
				break
			}
			// 如果未能获取图片地址, 则直接置为失败
			if !ep.FetchedPictures {
				println("章节的图片获取失败 " + downloadingComic.Title + " " + ep.Title)
				err = comic_center2.EpFailed(ep.ID)
				if err != nil {
					panic(err)
				}
			} else {
				println("章节的图片获取成功 " + downloadingComic.Title + " " + ep.Title)
				downloadingComic.SelectedPictureCount = downloadingComic.SelectedPictureCount + ep.SelectedPictureCount
				downloadComicEventSend(downloadingComic)
			}
		}
	}
	// 获取图片地址结束, 去初始化下载的章节
	go downloadLoadEp()
}

// 获取图片地址
func downloadFetchPictures(downloadEp *comic_center2.ComicDownloadEp) error {
	var list []comic_center2.ComicDownloadPicture
	// 官方的图片只能分页获取, 从第1页开始获取, 每页最多40张图片
	page := 1
	for true {
		rsp, err := client.ComicPicturePage(downloadingComic.ID, int(downloadEp.EpOrder), page)
		if err != nil {
			return err
		}
		for _, doc := range rsp.Docs {
			list = append(list, comic_center2.ComicDownloadPicture{
				ID:           doc.Id,
				ComicId:      downloadEp.ComicId,
				EpId:         downloadEp.ID,
				EpOrder:      downloadEp.EpOrder,
				OriginalName: doc.Media.OriginalName,
				FileServer:   doc.Media.FileServer,
				Path:         doc.Media.Path,
			})
		}
		// 如果不是最后一页, 页码加1, 获取下一页
		if rsp.Page < rsp.Pages {
			page++
			continue
		}
		break
	}
	// 保存获取到的图片
	err := comic_center2.FetchPictures(downloadEp.ComicId, downloadEp.ID, &list)
	if err != nil {
		panic(err)
	}
	downloadEp.SelectedPictureCount = int32(len(list))
	return err
}

// 初始化下载
func downloadLoadEp() {
	// 周期停止检测
	if downloadHasStop() {
		go downloadBegin()
		return
	}
	// 找到第一个需要下载的章节并去处理 （未下载失败的, 且未完成下载的）
	var err error
	downloadingEp, err = comic_center2.LoadFirstNeedDownloadEp(downloadingComic.ID)
	if err != nil {
		panic(err)
	}
	go downloadInitEp()
}

// 处理需要下载的EP
func downloadInitEp() {
	if downloadingEp == nil {
		// 所有Ep都下完了, 汇总Download下载情况
		go downloadSummaryDownload()
		return
	}
	// 没有下载完则去下载图片
	println("正在下载章节 " + downloadingEp.Title)
	go downloadLoadPicture()
}

// EP下载汇总
func downloadSummaryDownload() {
	// 暂停检测
	if downloadHasStop() {
		go downloadBegin()
		return
	}
	// 加载这个漫画的所有EP
	list, err := comic_center2.ListDownloadEpByComicId(downloadingComic.ID)
	if err != nil {
		panic(err)
	}
	// 判断所有章节是否下载完成
	over := true
	for _, downloadEp := range list {
		over = over && downloadEp.DownloadFinished
	}
	if over {
		// 如果所有章节下载完成则下载成功
		downloadAndExportLogo(downloadingComic)
		err = comic_center2.DownloadSuccess(downloadingComic.ID)
		if err != nil {
			panic(err)
		}
		downloadingComic.DownloadFinished = true
		downloadingComic.DownloadFinishedTime = time.Now()
	} else {
		// 否则下载失败
		err = comic_center2.DownloadFailed(downloadingComic.ID)
		if err != nil {
			panic(err)
		}
		downloadingComic.DownloadFailed = true
	}
	// 向前端发送下载状态
	downloadComicEventSend(downloadingComic)
	// 去下载下一个漫画
	go downloadLoadComic()
}

// 加载需要下载的图片
func downloadLoadPicture() {
	// 暂停检测
	if downloadHasStop() {
		go downloadBegin()
		return
	}
	// 获取到这个章节需要下载的图片
	downloadingPictures, err := comic_center2.LoadNeedDownloadPictures(downloadingEp.ID, downloadThreadFetch)
	if err != nil {
		panic(err)
	}
	// 如果不需要下载
	if len(*downloadingPictures) == 0 {
		// 所有图片都下完了, 汇总EP下载情况
		go downloadSummaryEp()
		return
	}
	// 线程池
	channel := make(chan int, downloadThreadCount)
	defer close(channel)
	wg := sync.WaitGroup{}
	for i := 0; i < len(*downloadingPictures); i++ {
		// 暂停检测
		if downloadHasStop() {
			wg.Wait()
			go downloadBegin()
			return
		}
		channel <- 0
		wg.Add(1)
		// 不放入携程, 防止i已经变化
		picPoint := &((*downloadingPictures)[i])
		go func() {
			downloadPicture(picPoint)
			<-channel
			wg.Done()
		}()
	}
	wg.Wait()
	// 再次新一轮的下载, 直至 len(*downloadingPictures) == 0
	go downloadLoadPicture()
}

var downloadEventChannelMutex = sync.Mutex{}

// 这里不能使用暂停检测, 多次检测会导致问题
func downloadPicture(downloadingPicture *comic_center2.ComicDownloadPicture) {
	// 下载图片, 最多重试5次
	println("正在下载图片 " + fmt.Sprintf("%d", downloadingPicture.RankInEp))
	for i := 0; i < 5; i++ {
		err := downloadThePicture(downloadingPicture)
		if err != nil {
			continue
		}
		func() {
			downloadEventChannelMutex.Lock()
			defer downloadEventChannelMutex.Unlock()
			// 对下载的漫画临时变量热更新并通知前端
			downloadingPicture.DownloadFinished = true
			downloadingEp.DownloadPictureCount = downloadingEp.DownloadPictureCount + 1
			downloadingComic.DownloadPictureCount = downloadingComic.DownloadPictureCount + 1
			downloadComicEventSend(downloadingComic)
		}()
		break
	}
	// 没能下载成功, 图片置为下载失败
	if !downloadingPicture.DownloadFinished {
		err := comic_center2.PictureFailed(downloadingPicture.ID)
		if err != nil {
			// ??? panic X channel ???
			// panic(err)
		}
	}
}

// 下载指定图片
func downloadThePicture(picturePoint *comic_center2.ComicDownloadPicture) error {
	// 为了不和页面前端浏览的数据冲突, 使用url做hash锁
	lock := utils2.HashLock(fmt.Sprintf("%s$%s", picturePoint.FileServer, picturePoint.Path))
	lock.Lock()
	defer lock.Unlock()
	// 图片保存位置使用相对路径储存, 使用绝对路径操作
	picturePath := fmt.Sprintf("%s/%d/%d", picturePoint.ComicId, picturePoint.EpOrder, picturePoint.RankInEp)
	realPath := downloadPath(picturePath)
	// 从缓存获取图片
	buff, img, format, err := decodeFromCache(picturePoint.FileServer, picturePoint.Path)
	if err != nil {
		// 若缓存不存在, 则从网络获取
		buff, img, format, err = decodeFromUrl(picturePoint.FileServer, picturePoint.Path)
	}
	if err != nil {
		return err
	}
	// 将图片保存到文件
	dir := filepath.Dir(realPath)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		os.Mkdir(dir, utils2.CreateDirMode)
	}
	err = ioutil.WriteFile(downloadPath(picturePath), buff, utils2.CreateFileMode)
	if err != nil {
		return err
	}
	// 下载时同时导出
	downloadAndExport(downloadingComic, downloadingEp, picturePoint, buff, format)
	// 存入数据库
	return comic_center2.PictureSuccess(
		picturePoint.ComicId,
		picturePoint.EpId,
		picturePoint.ID,
		int64(len(buff)),
		format,
		int32(img.Bounds().Dx()),
		int32(img.Bounds().Dy()),
		picturePath,
	)
}

// EP 下载内容汇总
func downloadSummaryEp() {
	// 暂停检测
	if downloadHasStop() {
		go downloadBegin()
		return
	}
	// 找到所有下载的图片
	list, err := comic_center2.ListDownloadPictureByEpId(downloadingEp.ID)
	if err != nil {
		panic(err)
	}
	// 全部下载完成置为成功, 否则置为失败
	over := true
	for _, downloadPicture := range list {
		over = over && downloadPicture.DownloadFinished
	}
	if over {
		err = comic_center2.EpSuccess(downloadingEp.ComicId, downloadingEp.ID)
		if err != nil {
			panic(err)
		}
	} else {
		err = comic_center2.EpFailed(downloadingEp.ID)
		if err != nil {
			panic(err)
		}
	}
	// 去加载下一个EP
	go downloadLoadEp()
}

// 边下载边导出(导出路径)
var downloadAndExportPath = ""

// 边下载边导出(导出图片)
func downloadAndExport(
	downloadingComic *comic_center2.ComicDownload,
	downloadingEp *comic_center2.ComicDownloadEp,
	downloadingPicture *comic_center2.ComicDownloadPicture,
	buff []byte,
	format string,
) {
	if downloadAndExportPath == "" {
		return
	}
	if i, e := os.Stat(downloadAndExportPath); e == nil {
		if i.IsDir() {
			// 进入漫画目录
			comicDir := path.Join(downloadAndExportPath, utils2.ReasonableFileName(downloadingComic.Title))
			i, e = os.Stat(comicDir)
			if e != nil {
				if os.IsNotExist(e) {
					e = os.Mkdir(comicDir, utils2.CreateDirMode)
				} else {
					return
				}
			}
			if e != nil {
				return
			}
			// 进入章节目录
			epDir := path.Join(comicDir, utils2.ReasonableFileName(fmt.Sprintf("%02d - ", downloadingEp.EpOrder)+downloadingEp.Title))
			i, e = os.Stat(epDir)
			if e != nil {
				if os.IsNotExist(e) {
					e = os.Mkdir(epDir, utils2.CreateDirMode)
				} else {
					return
				}
			}
			if e != nil {
				return
			}
			// 写入文件
			filePath := path.Join(epDir, fmt.Sprintf("%03d.%s", downloadingPicture.RankInEp, aliasFormat(format)))
			ioutil.WriteFile(filePath, buff, utils2.CreateFileMode)
		}
	}
}

// 边下载边导出(导出logo)
func downloadAndExportLogo(
	downloadingComic *comic_center2.ComicDownload,
) {
	if downloadAndExportPath == "" {
		return
	}
	comicLogoPath := downloadPath(path.Join(downloadingComic.ID, "logo"))
	if _, e := os.Stat(comicLogoPath); e == nil {
		buff, e := ioutil.ReadFile(comicLogoPath)
		if e == nil {
			_, f, e := image.Decode(bytes.NewBuffer(buff))
			if e == nil {
				if i, e := os.Stat(downloadAndExportPath); e == nil {
					if i.IsDir() {
						// 进入漫画目录
						comicDir := path.Join(downloadAndExportPath, utils2.ReasonableFileName(downloadingComic.Title))
						i, e = os.Stat(comicDir)
						if e != nil {
							if os.IsNotExist(e) {
								e = os.Mkdir(comicDir, utils2.CreateDirMode)
							}
						}
						if e != nil {
							return
						}
						// 写入文件
						filePath := path.Join(comicDir, fmt.Sprintf("%s.%s", "logo", aliasFormat(f)))
						ioutil.WriteFile(filePath, buff, utils2.CreateFileMode)
					}
				}
			}
		}
	}
}

// jpeg的拓展名
func aliasFormat(format string) string {
	if format == "jpeg" {
		return "jpg"
	}
	return format
}
