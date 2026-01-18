import { useState, useEffect } from 'react';
import { useParams, Link } from 'react-router-dom';
import { groupsAPI } from '../services/api';
import { formatBytes } from '../utils/formatBytes';
import Navbar from '../components/Navbar';
import Footer from '../components/Footer';
import './GroupPage.css';

function GroupPage() {
  const { groupName } = useParams();
  const [data, setData] = useState(null);
  const [isLoading, setIsLoading] = useState(true);
  const [isRefreshing, setIsRefreshing] = useState(false);

  const loadData = async () => {
    try {
      const decodedGroup = decodeURIComponent(groupName);
      const groupData = await groupsAPI.getRepositories(decodedGroup);
      setData(groupData);
    } catch (error) {
      console.error('Failed to load group data:', error);
    } finally {
      setIsLoading(false);
      setIsRefreshing(false);
    }
  };

  useEffect(() => {
    loadData();
  }, [groupName]);

  const handleRefresh = () => {
    setIsRefreshing(true);
    loadData();
  };


  if (isLoading) {
    return (
      <div className="d-flex justify-content-center align-items-center" style={{ minHeight: '100vh' }}>
        <div className="spinner-border text-primary" role="status">
          <span className="visually-hidden">Loading...</span>
        </div>
      </div>
    );
  }

  const decodedGroup = decodeURIComponent(groupName);

  return (
    <div className="container-main">
      <Navbar onRefresh={handleRefresh} isRefreshing={isRefreshing} title={decodedGroup} />

      <div className="container-fluid px-0">
        <div className="card">
          <div className="card-header">
            <div className="card-header-content">
              <div className="card-header-title">
                <Link to="/" className="nav-link-back">
                  <i className="bi bi-arrow-left"></i>
                </Link>
                <h5>
                  <i className="bi bi-folder"></i>
                  {decodedGroup}
                </h5>
              </div>
              <small>List of repositories in this group</small>
            </div>
          </div>
          <div className="card-body">
            {data?.repositories && data.repositories.length > 0 ? (
              <div className="row g-3">
                {data.repositories.map((repo) => {
                  const totalSize = repo.tags.reduce((sum, tag) => sum + tag.size, 0);
                  return (
                    <div key={repo.name} className="col-md-4">
                      <Link
                        to={`/group/${encodeURIComponent(data.group)}/repository/${encodeURIComponent(repo.name)}`}
                        className="text-decoration-none"
                      >
                        <div className="card h-100 repository-card">
                          <div className="card-body">
                            <h6 className="card-title">
                              <i className="bi bi-box-seam me-2 text-primary"></i>
                              {repo.name}
                            </h6>
                            <p className="card-text text-muted mb-1">
                              <i className="bi bi-tag me-1"></i>
                              {repo.tags.length} {repo.tags.length === 1 ? 'tag' : 'tags'}
                            </p>
                            <p className="card-text text-muted mb-0">
                              <i className="bi bi-hdd me-1"></i>
                              {formatBytes(totalSize)}
                            </p>
                          </div>
                        </div>
                      </Link>
                    </div>
                  );
                })}
              </div>
            ) : (
              <div className="empty-state">
                <i className="bi bi-box-seam"></i>
                <p className="mb-0">No repositories found in this group.</p>
              </div>
            )}
          </div>
        </div>
      </div>
      
      <Footer />
    </div>
  );
}

export default GroupPage;

