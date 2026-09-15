#include <sys/socket.h>
#include <netinet/in.h>
#include <unistd.h>
#include <stdio.h>
#include <string.h>
extern const char *message(void);
int main(void) {
 int s=socket(AF_INET,SOCK_STREAM,0),one=1;setsockopt(s,SOL_SOCKET,SO_REUSEADDR,&one,sizeof(one));
 struct sockaddr_in a={.sin_family=AF_INET,.sin_port=htons(8080),.sin_addr.s_addr=0};
 if(bind(s,(struct sockaddr*)&a,sizeof(a))||listen(s,16))return 2;
 puts("DYNAMIC_READY");fflush(stdout);
 for(;;){int c=accept(s,0,0);if(c<0)continue;char buf[2048];read(c,buf,sizeof(buf));const char *body=message();int n=snprintf(buf,sizeof(buf),"HTTP/1.1 200 OK\r\nContent-Length: %zu\r\nConnection: close\r\n\r\n%s",strlen(body),body);write(c,buf,n);close(c);}
}
